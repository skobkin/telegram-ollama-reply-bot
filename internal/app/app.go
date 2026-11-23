package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"telegram-ollama-reply-bot/internal/adminconfig"
	core "telegram-ollama-reply-bot/internal/adminui"
	"telegram-ollama-reply-bot/internal/buildinfo"
	"telegram-ollama-reply-bot/internal/config"
	"telegram-ollama-reply-bot/internal/content/extractor"
	"telegram-ollama-reply-bot/internal/content/fetch"
	"telegram-ollama-reply-bot/internal/content/search"
	"telegram-ollama-reply-bot/internal/execution"
	"telegram-ollama-reply-bot/internal/interactivity"
	"telegram-ollama-reply-bot/internal/llm"
	"telegram-ollama-reply-bot/internal/logging"
	"telegram-ollama-reply-bot/internal/mcp"
	psqlite "telegram-ollama-reply-bot/internal/persistence/sqlite"
	"telegram-ollama-reply-bot/internal/provider"
	"telegram-ollama-reply-bot/internal/reminders"
	"telegram-ollama-reply-bot/internal/state/memory"
	"telegram-ollama-reply-bot/internal/statistics"
	"telegram-ollama-reply-bot/internal/statistics/pngreport"
	"telegram-ollama-reply-bot/internal/stickers"
	"telegram-ollama-reply-bot/internal/support/httpclient"
	"telegram-ollama-reply-bot/internal/support/markdown"
	"telegram-ollama-reply-bot/internal/support/safeerr"
	"telegram-ollama-reply-bot/internal/support/secretscrub"
	"telegram-ollama-reply-bot/internal/systemone"
	adminui "telegram-ollama-reply-bot/internal/telegram/adminui"
	"telegram-ollama-reply-bot/internal/telegram/bot"
	"telegram-ollama-reply-bot/internal/telegram/reactionavailability"
	"telegram-ollama-reply-bot/internal/telegram/transport"
	"telegram-ollama-reply-bot/internal/tooluse"
	"telegram-ollama-reply-bot/internal/vision"
	"time"

	"github.com/getsentry/sentry-go"
	tg "github.com/mymmrac/telego"
)

// ErrTelegramConnect reports a failed Telegram API identity check. The
// underlying transport error is never retained: telego's request errors embed
// the full API URL, which carries the bot token in its path.
var ErrTelegramConnect = errors.New("cannot connect to telegram api")

// modelResolver adapts operator-managed settings to llm.ModelResolver: chat
// resolves per chat, summarize and image recognition are global-only
// features whose values are the same for every chat. Unknown features have
// no runtime override and resolve as unset.
type modelResolver struct {
	admin *adminconfig.Service
}

func (r modelResolver) ResolveModel(ctx context.Context, feature llm.Feature, chatID int64) (string, error) {
	resolved, err := r.admin.ResolveChatConfig(ctx, chatID)
	if err != nil {
		return "", err
	}

	switch feature {
	case llm.FeatureChat:
		return resolved.ChatModel, nil
	case llm.FeatureSummarize:
		return resolved.SummarizeModel, nil
	case llm.FeatureImageRecognition:
		return resolved.ImageRecognitionModel, nil
	default:
		return "", nil
	}
}

// decisionPersonaSource renders the decision context's persona through the
// ordinary admin prompt path: the full effective character prompt the
// generation turns use, from the persona the operation captured at start.
// The generation model passed in comes from the operation snapshot, so any
// model-identity reference in the persona describes the generation
// character; an empty snapshot model falls back to the environment chat
// route — the same fallback ordinary prompt rendering applies — so an unset
// override chain never renders an empty model identity. The compact context
// stays empty — the decision state already carries the conversation as
// bounded history — and the tool-policy section states that no tools exist
// in a decision.
type decisionPersonaSource struct {
	prompts           *llm.AdminPromptRenderer
	fallbackChatModel string
}

// decisionPersonaToolPolicy renders the chat template's tool-policy section
// for decision contexts: the decision backend receives no tools, and the
// persona must not advertise any.
const decisionPersonaToolPolicy = "No tools are available in this context."

func (s decisionPersonaSource) DecisionPersona(ctx context.Context, chatID int64, generationModel string, settings interactivity.PersonaSettings) (string, error) {
	if generationModel == "" {
		generationModel = s.fallbackChatModel
	}

	return s.prompts.RenderCapturedChatSystemPrompt(ctx, llm.CapturedPersona{
		TemplateBody:  settings.TemplateBody,
		Language:      settings.Language,
		Gender:        settings.Gender,
		CharacterName: settings.CharacterName,
	}, chatID, generationModel, "", decisionPersonaToolPolicy)
}

// CapturePersonaTemplate resolves the chat prompt template body of the
// operation snapshot: the body the decision persona renders from and the
// action turn renders from, identical even if the template changes in
// between.
func (s decisionPersonaSource) CapturePersonaTemplate(ctx context.Context, chatID int64) (string, error) {
	return s.prompts.ChatPromptBody(ctx, chatID)
}

// envChatRouteModel resolves the environment chat-route model once at
// wiring time, mirroring the route fallback the generation path applies
// when the runtime override chain is unset. A route error yields "" — the
// boot-time model checks fail loudly on invalid routes, so this only
// degrades the persona's model identity text.
func envChatRouteModel(cfg config.LLMConfig) string {
	route, err := cfg.RouteForFeature("chat")
	if err != nil {
		return ""
	}

	return route.Model
}

// verifyStartupModels checks that every required feature model is available
// on the service that will actually serve it. The main service always covers
// chat and summarize. The image-recognition model is appended to the main
// check when recognition shares the conversational connection (image is nil),
// checked against the separately configured image service when one exists,
// and not checked at all when imageCheck is false — a disabled feature or a
// model-free recognizer must not require an unused image LLM model.
func verifyStartupModels(ctx context.Context, main, image *llm.Service, imageCheck bool) (bool, map[string]bool) {
	features := []llm.Feature{llm.FeatureChat, llm.FeatureSummarize}

	if !imageCheck {
		return main.HasModelsFor(ctx, features...)
	}

	if image == nil {
		return main.HasModelsFor(ctx, append(features, llm.FeatureImageRecognition)...)
	}

	hasMain, mainResult := main.HasModelsFor(ctx, features...)
	hasImage, imageResult := image.HasModelsFor(ctx, llm.FeatureImageRecognition)

	for feature, available := range imageResult {
		mainResult[feature] = available
	}

	return hasMain && hasImage, mainResult
}

// authProbeTimeout bounds one startup credential probe. It must stay well
// under a tolerable boot delay while allowing slow providers to answer.
const authProbeTimeout = 15 * time.Second

// authValidator probes external-service credentials with a minimal real API
// call. Implemented optionally by the search backends and the MiniMax
// recognizer; plain interfaces stay untouched.
type authValidator interface {
	ValidateAuth(ctx context.Context) error
}

// verifyRecognizerAuth validates the active image recognizer's credentials
// when it implements authValidator. A rejected key (ErrRecognizerAuth) is a
// definitive misconfiguration and returns an error to abort the boot; any
// other probe failure is transient (the probe call itself may fail for
// reasons unrelated to the key) and only logs — recognition may still work
// at runtime.
func verifyRecognizerAuth(ctx context.Context, recognizer vision.Recognizer, logger *slog.Logger) error {
	validator, ok := recognizer.(authValidator)
	if !ok {
		return nil
	}

	probeCtx, cancel := context.WithTimeout(ctx, authProbeTimeout)
	defer cancel()

	err := validator.ValidateAuth(probeCtx)
	if err == nil {
		return nil
	}
	if errors.Is(err, vision.ErrRecognizerAuth) {
		return err
	}

	logger.Error("image recognizer auth validation failed, continuing", "recognizer", recognizer.Name(), "error", err)

	return nil
}

// verifySearcherAuth validates the configured search backend's credentials
// when it implements authValidator. Definitive rejections (auth failed,
// credits exhausted) return an error to abort the boot; transient failures
// only log, and a searcher without a validator (or a disabled backend) is
// skipped.
func verifySearcherAuth(ctx context.Context, searcher search.Searcher, logger *slog.Logger) error {
	if searcher == nil {
		return nil
	}

	validator, ok := searcher.(authValidator)
	if !ok {
		return nil
	}

	probeCtx, cancel := context.WithTimeout(ctx, authProbeTimeout)
	defer cancel()

	err := validator.ValidateAuth(probeCtx)
	if err == nil {
		return nil
	}
	if provider.IsDefinitiveRejection(err) {
		return err
	}

	logger.Error("search provider auth validation failed, continuing", "error", err)

	return nil
}

// verifyTelegramIdentity resolves the bot's own identity, logging the
// outcome. A failed GetMe is sanitized before it is logged or returned:
// telego's transport errors embed the token-bearing API URL, and the telego
// logger's redaction does not cover this path. The caller may report the
// returned error to Sentry and to the startup failure as-is.
func verifyTelegramIdentity(ctx context.Context, api *tg.Bot, logger *slog.Logger) (*tg.User, error) {
	botUser, err := api.GetMe(ctx)
	if err != nil {
		sanitized := safeerr.WrapTransport(ErrTelegramConnect, err)
		logger.Error("telegram api connect failed", "error", sanitized)

		return nil, sanitized
	}

	logger.Info("telegram api connected", "bot_id", botUser.ID, "bot_username", botUser.Username)

	return botUser, nil
}

// botIdentityFromUser converts the verified startup identity into the
// tool runtime's actor identity, with the same display-name derivation the
// bot package uses for its own stored replies.
func botIdentityFromUser(botUser *tg.User) tooluse.BotIdentity {
	if botUser == nil {
		return tooluse.BotIdentity{}
	}

	name := strings.TrimSpace(botUser.FirstName + " " + botUser.LastName)
	if name == "" {
		name = botUser.Username
	}

	return tooluse.BotIdentity{
		UserID:   botUser.ID,
		Name:     name,
		Username: botUser.Username,
	}
}

// imageModelLogValue returns the value logged as image_model: the configured
// model on an OpenAI-compatible route, or an empty string when the active
// recognizer does not use one (the recognizer name is logged separately).
func imageModelLogValue(cfg *config.Config) string {
	if !cfg.LLM.ImageRecognitionEnabled || cfg.LLM.ImageRecognition.Recognizer != config.ImageRecognizerOpenAICompat {
		return ""
	}

	return cfg.LLM.Features.ImageRecognition.Model
}

// attachImageTools wires the model-prompted image analysis tools onto the
// runtime only while image recognition is enabled; while disabled the tools
// are absent from the registry and never advertised to the model.
func attachImageTools(tools *tooluse.Runtime, analyzer tooluse.ImageAnalyzer, enabled bool) {
	if !enabled {
		return
	}

	tools.AttachImageAnalyzer(analyzer)
}

func Run(ctx context.Context) (runErr error) {
	// Bootstrap scrubbing: a failed config.Load returns before the full
	// registry exists, and main prints the escaping error to plain stderr.
	// The environment-derived subset registers the credentials Load would
	// have, so a failed boot cannot echo them either. Both deferred scrubs
	// run on the way out (last installed first); scrubbing is idempotent.
	bootstrapScrubber := secretscrub.New(bootstrapEnvSecrets())
	defer func() {
		runErr = scrubFatalError(bootstrapScrubber, runErr)
	}()

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	// The scrubber is built before any logging or telemetry exists, so every
	// sink — log records, Sentry events, HTTP client errors — is covered
	// from the first line of output onward.
	scrubber := newSecretScrubber(cfg)

	// The process-exit diagnostic is printed by main outside every scrubbed
	// sink (plain stderr), so the error escaping Run is scrubbed here before
	// it gets there. Chain identity is irrelevant at the process boundary:
	// main only prints the message and exits.
	defer func() {
		runErr = scrubFatalError(scrubber, runErr)
	}()

	logManager, err := logging.NewManager(logging.Options{
		Level:      cfg.Logging.Level,
		SetDefault: true,
		Scrubber:   scrubber,
	})
	if err != nil {
		return fmt.Errorf("configure logging: %w", err)
	}
	logger := logManager.Logger("app")
	logger.Info("starting app", "app", buildinfo.AppName, "version", buildinfo.Version)

	if cfg.Sentry.DSN != "" {
		logger.Debug("initializing sentry")

		err = sentry.Init(sentry.ClientOptions{
			Dsn:              cfg.Sentry.DSN,
			AttachStacktrace: true,
			BeforeSend:       newSentryBeforeSend(scrubber),
		})
		if err != nil {
			logger.Error("sentry initialization failed", "error", err)
		} else {
			logger.Info("sentry enabled")
			defer sentry.Flush(2 * time.Second)
		}
	} else {
		logger.Info("sentry disabled")
	}

	logger.Info(
		"selected llm models",
		"backend", config.LLMBackendOpenAICompat,
		"chat_model", cfg.LLM.Features.Chat.Model,
		"summarize_model", cfg.LLM.Features.Summarize.Model,
		"image_model", imageModelLogValue(cfg),
		"image_recognizer", cfg.LLM.ImageRecognition.Recognizer,
	)
	transport.LogNetworkRouting(logger, cfg)

	// Telegram identity is resolved before any wiring that needs it: the bot
	// handle is baked into the prompt renderer below, and bot.Run performs its
	// own (idempotent) GetMe later.
	telegramAPI, err := tg.NewBot(cfg.Bot.Telegram.Token, tg.WithLogger(bot.NewLogger(
		logManager.Logger("telegram/bot").With("source", "telego"),
		cfg.Bot.Telegram.Token,
	)), tg.WithHTTPClient(httpclient.New()))
	if err != nil {
		logger.Error("telegram api initialization failed", "error", err)
		sentry.CaptureException(err)

		return err
	}

	botUser, err := verifyTelegramIdentity(ctx, telegramAPI, logger)
	if err != nil {
		sentry.CaptureException(err)

		return err
	}

	persistentStore, err := psqlite.Open(ctx, cfg.Persistence.StorePath, logManager.Logger("persistence/sqlite"))
	if err != nil {
		logger.Error("failed to initialize persistent store", "error", err)
		sentry.CaptureException(err)

		return err
	}
	defer func() { _ = persistentStore.Close() }()

	// The operator-curated sticker catalog is restored all-or-nothing before
	// any consumer (admin UI, model tools) exists: a corrupt persisted row
	// aborts boot rather than advertising a partial or unvalidated catalog.
	stickerService := stickers.NewService(
		stickerCatalogStore{store: persistentStore},
		telegoStickerFetcher{api: telegramAPI},
		logManager.Logger("stickers"),
		stickers.WithFileDownloader(telegoStickerFiles{api: telegramAPI, client: newStickerFileClient()}),
	)
	if err := stickerService.Load(ctx); err != nil {
		logger.Error("failed to restore sticker catalog", "error", err)
		sentry.CaptureException(err)

		return err
	}

	adminService := adminconfig.NewService(persistentStore, adminconfig.BuiltinTemplates{
		adminconfig.PromptFeatureChat:             llm.DefaultChatPromptTemplate,
		adminconfig.PromptFeatureSummarize:        llm.DefaultSummarizePromptTemplate,
		adminconfig.PromptFeatureImageRecognition: llm.DefaultImageRecognitionPromptTemplate,
	}, llm.ValidatePromptTemplate)

	prompts := llm.NewAdminPromptRenderer(adminService, cfg.LLM.ImageRecognitionEnabled, botUser.Username)

	resolver := modelResolver{admin: adminService}

	// System One decision backend: the client is shared by the admin UI model
	// picker (and, later in the wiring, the interaction engine and decision
	// tools). An empty endpoint yields an unconfigured client — picker
	// discovery unavailable and decision features off, never a boot failure.
	systemOneClient, err := systemone.New(systemone.Config{
		BaseURL:          cfg.SystemOne.BaseURL,
		APIKey:           cfg.SystemOne.APIKey,
		Timeout:          cfg.SystemOne.Timeout,
		MaxResponseBytes: cfg.SystemOne.MaxResponseBytes,
		InputBudgetBytes: cfg.SystemOne.InputBudgetBytes,
	}, logManager.Logger("systemone"))
	if err != nil {
		logger.Error("failed to initialize system one client", "error", err)
		sentry.CaptureException(err)

		return err
	}

	// One scrubbing doer shared by both LLM services: go-openai is the only
	// consumer that accepts an injectable client interface, so its transport
	// errors (which would otherwise embed URLs) are scrubbed at the source.
	llmHTTP := secretscrub.WrapDoer(scrubber, httpclient.New())

	llmc, err := llm.NewService(cfg.LLM, prompts, resolver, llmHTTP, logManager.Logger("llm"))
	if err != nil {
		logger.Error("failed to initialize llm service", "error", err)
		sentry.CaptureException(err)

		return err
	}

	// Image-recognition wiring: the recognizer is selected by configuration.
	// The OpenAI-compatible route either shares the conversational service or
	// gets a separately configured one; the MiniMax VLM route is model-free.
	// Construction errors (missing key, bad URL) refuse the boot with
	// config-path-naming errors, mirroring llm.NewService.
	var imageLLM *llm.Service

	var recognizer vision.Recognizer

	switch {
	case cfg.LLM.ImageRecognition.Recognizer == config.ImageRecognizerMiniMaxVLM && cfg.LLM.ImageRecognitionEnabled:
		minimaxRecognizer, err := vision.NewMiniMaxRecognizer(
			cfg.LLM.ImageRecognition.MiniMaxVLM.BaseURL,
			cfg.LLM.ImageRecognition.MiniMaxVLM.APIKey,
			cfg.LLM.ImageRecognition.MiniMaxVLM.Timeout,
			logManager.Logger("vision/minimax"),
		)
		if err != nil {
			logger.Error("failed to initialize minimax vlm recognizer", "error", err)
			sentry.CaptureException(err)

			return err
		}

		recognizer = minimaxRecognizer
	case cfg.LLM.ImageRecognition.OpenAICompat.BaseURL != "":
		imageCfg := cfg.LLM
		imageCfg.Backends.OpenAICompat = config.OpenAICompatBackendConfig{
			BaseURL:  cfg.LLM.ImageRecognition.OpenAICompat.BaseURL,
			APIToken: cfg.LLM.ImageRecognition.OpenAICompat.APIToken,
		}

		imageLLM, err = llm.NewService(imageCfg, prompts, resolver, llmHTTP, logManager.Logger("llm/image"))
		if err != nil {
			logger.Error("failed to initialize image llm service", "error", err)
			sentry.CaptureException(err)

			return err
		}

		recognizer = vision.NewOpenAIRecognizer(imageLLM, resolver)
	default:
		recognizer = vision.NewOpenAIRecognizer(llmc, resolver)
	}

	visionService := vision.NewService(
		recognizer,
		prompts,
		cfg.LLM.ImageRecognition.MaxImageBytes,
		logManager.Logger("vision"),
	)

	logger.Debug("checking models availability")

	// The image-recognition model is only checked when the active recognizer
	// routes through one (a disabled feature or the model-free MiniMax route
	// must not require an unused image LLM model), and it is checked against
	// the connection that will actually serve it.
	imageModelCheck := cfg.LLM.ImageRecognitionEnabled && visionService.RequiresModel()

	hasAll, searchResult := verifyStartupModels(ctx, llmc, imageLLM, imageModelCheck)
	if !hasAll {
		logger.Error("required models are unavailable", "result", searchResult)
		sentry.CaptureMessage("Not all models are available")

		return fmt.Errorf("missing required models: %v", searchResult)
	}

	logger.Info("all required models are available")

	// Credential probes for the services without a cheaper existing check:
	// the model listing above already validates the chat/image LLM tokens,
	// and the MiniMax VLM key is only exercised here.
	if err := verifyRecognizerAuth(ctx, recognizer, logger); err != nil {
		logger.Error("image recognizer rejected credentials", "error", err)
		sentry.CaptureException(err)

		return err
	}

	contentFetcher, err := fetch.New(fetch.Options{
		Timeout:      cfg.Content.Fetch.Timeout,
		MaxBodyBytes: cfg.Content.Fetch.MaxBodyBytes,
		MaxRedirects: cfg.Content.Fetch.MaxRedirects,
		UserAgent:    cfg.Content.Fetch.UserAgent,
		DenyDomains:  cfg.Content.Fetch.DenyDomains,
		DenyCIDRs:    cfg.Content.Fetch.DenyCIDRs,
	}, logManager.Logger("content/fetch"))
	if err != nil {
		logger.Error("failed to initialize content fetcher", "error", err)
		sentry.CaptureException(err)

		return err
	}

	extractBackends, err := extractor.NewBackends(cfg.Content.Extract.Backends, logManager.Logger("content/extractor"))
	if err != nil {
		logger.Error("failed to initialize extraction backends", "error", err)
		sentry.CaptureException(err)

		return err
	}

	ext := extractor.NewPipeline(contentFetcher, extractBackends, logManager.Logger("content/extractor"))
	searcher, err := search.NewFromConfig(*cfg, httpclient.New(), logManager.Logger("content/search"))
	if err != nil {
		logger.Error("failed to initialize search capability", "error", err)
		sentry.CaptureException(err)

		return err
	}
	if err := verifySearcherAuth(ctx, searcher, logger); err != nil {
		logger.Error("search provider rejected credentials", "error", err)
		sentry.CaptureException(err)

		return err
	}
	stores := memory.New(memory.Config{
		MaxBytes:           cfg.State.MaxBytes,
		HistoryMaxBytes:    cfg.State.HistoryMaxBytes,
		HistoryStreamsMax:  cfg.State.HistoryStreamsMax,
		ImageCacheMaxBytes: cfg.State.ImageCacheMaxBytes,
		ImageCacheTTL:      cfg.State.ImageCacheTTL,
	}, logManager.Logger("state/memory"))

	sanitizer := markdown.NewTgMarkdownV2Sanitizer()

	// Execution event dispatcher: publishes run/tool lifecycle and chat
	// interaction events to in-process subscribers (the live preview
	// renderer, the admin UI menu re-creation). Nothing is persisted; no
	// authoritative result depends on it. Created before the services that
	// publish through the event-wrapped Telegram sender below.
	dispatcher := execution.NewDispatcher(execution.DispatcherConfig{QueueSize: 256}, logManager.Logger("execution"))
	dispatcher.Start(ctx)

	// Telegram sends that can land in a chat the bot serves are wrapped so
	// every visible message publishes MessageSent: reminders delivered into
	// admin DMs, and the model-authored poll/quiz/dice actions.
	eventAPI := &eventPublishingBot{api: telegramAPI, events: dispatcher}

	// Reminder delivery respects the access policy. Admin DMs are always
	// deliverable (positive chat IDs are private chats; only admin-owned ones
	// are exempt), ordinary chats go through the same decision as messages.
	reminderService := reminders.NewService(persistentStore, eventAPI, cfg.Bot.AdminIDs, func(ctx context.Context, chatID int64) (bool, error) {
		if chatID > 0 && slices.Contains(cfg.Bot.AdminIDs, chatID) {
			return true, nil
		}

		return adminService.ShouldAllowChat(ctx, chatID, false)
	}, logManager.Logger("reminders"))
	go func() {
		if err := reminderService.Run(ctx); err != nil {
			logger.Error("reminder scheduler exited with error", "error", err)
			sentry.CaptureException(err)
		}
	}()
	mcpManager, err := mcp.NewFromConfig(ctx, cfg.MCP, logManager.Logger("mcp"))
	if err != nil {
		logger.Error("failed to initialize mcp manager", "error", err)
		sentry.CaptureException(err)

		return err
	}

	tools := tooluse.New(
		llmc,
		stores.Conversations(),
		ext,
		searcher,
		eventAPI,
		reminderService,
		logManager.Logger("tooluse"),
		tooluse.Config{
			MaxIterations:      cfg.LLM.ToolLoopMaxIterations,
			AdminIDs:           cfg.Bot.AdminIDs,
			RecentHistoryLimit: cfg.Bot.UncompressedHistoryLimit,
			BotIdentity:        botIdentityFromUser(botUser),
			Dispatcher:         dispatcher,
		},
	)
	tools.AttachReactionAvailability(reactionavailability.New(telegramAPI, logManager.Logger("reactionavailability")))
	tools.AttachStickerCatalog(stickerService)
	tools.AttachMCP("mcp", mcpManager)
	// Decision tools are registered statically when the System One backend
	// is configured; their per-turn availability additionally requires an
	// effective System One model from the operation snapshot.
	if systemOneClient.Configured() {
		tools.AttachDecisionTools(systemOneClient)
	}
	defer tools.Close()
	statsCollector := statistics.NewCollector(statistics.Config{
		RubricVersion: interactivity.RubricVersion,
	})
	// Persistent statistics datasets are restored synchronously before any
	// producer (bot handlers, admin UI) can record, and flushed on a ticker.
	// The persister is stopped before the store closes: it is deferred after
	// the store's defer, so LIFO runs it first on every exit path. The
	// ordinary shutdown path below stops it explicitly after every producer
	// has been joined.
	statsPersister := statistics.NewPersister(statistics.PersisterConfig{
		Store:  statisticsSnapshotStore{store: persistentStore},
		Logger: logManager.Logger("statistics/persistence"),
		Report: func(err error) { sentry.CaptureException(err) },
	}, statsCollector.InteractionDataset(), statsCollector.ToolDataset())
	if err := statsPersister.Restore(ctx); err != nil {
		logger.Error("failed to restore persistent statistics", "error", err)
		sentry.CaptureException(err)

		return err
	}
	statsPersister.Start(ctx)
	defer func() { _ = statsPersister.Stop() }()
	botService := bot.NewBot(
		ctx,
		telegramAPI,
		llmc,
		visionService,
		ext,
		sanitizer,
		stores.Conversations(),
		stores.Images(),
		statsCollector,
		cfg.Bot,
		adminService,
		tools,
		logManager.Logger("telegram/bot"),
		bot.ImageRecognitionSettings{
			Enabled:       cfg.LLM.ImageRecognitionEnabled,
			MaxImageBytes: cfg.LLM.ImageRecognition.MaxImageBytes,
		},
	)

	// The scheduler and tools can record bot messages before Run has resolved
	// the identity itself; seed it from the one verified at startup.
	botService.SetIdentity(botUser)

	// Interactive admin UI: controller (sessions, screens) plus the Telegram
	// adapter that renders it. Ephemeral by design — sessions and pendings
	// live in memory only. Model pickers discover from the backend actually
	// serving each feature: the main service for chat/summarize, the
	// dedicated image service when one is configured (else the shared
	// connection), the System One client when the decision backend is
	// configured. The image model setting is hidden while the model-free
	// MiniMax recognizer is active.
	var controllerOpts []core.ControllerOption
	imageDiscovery := imageLLM
	if imageDiscovery == nil {
		imageDiscovery = llmc
	}
	// Generation, summarize, and image pickers discover from their serving
	// backends regardless of the decision backend: the optional System One
	// client contributes only its own provider (an untyped nil when
	// unconfigured — that field's picker falls back to manual entry).
	var systemOneDiscovery core.ModelDiscovery
	if systemOneClient.Configured() {
		systemOneDiscovery = systemOneClient
	}
	controllerOpts = append(controllerOpts, core.WithModelDiscovery(llmc, imageDiscovery, systemOneDiscovery))
	controllerOpts = append(controllerOpts, core.WithStickerAdmin(stickerService))
	controllerOpts = append(controllerOpts, core.WithStatsReport(pngreport.Render), core.WithToolStatsReport(pngreport.RenderTools), core.WithStatsPersistence(statsPersister))
	if cfg.LLM.ImageRecognitionEnabled && cfg.LLM.ImageRecognition.Recognizer == config.ImageRecognizerMiniMaxVLM {
		controllerOpts = append(controllerOpts, core.WithHiddenImageModel())
	}
	if searcher == nil {
		// The web search permission only narrows a configured backend; with
		// none, the setting would be a no-op that suggests otherwise.
		controllerOpts = append(controllerOpts, core.WithHiddenWebSearch())
	}
	adminUIManager := core.NewManager(nil)
	adminUIController := core.NewController(adminUIManager, adminService, cfg.Bot.AdminIDs, statsCollector, controllerOpts...)
	adminAdapter := adminui.NewAdapter(
		adminUIController,
		adminUIManager,
		cfg.Bot.AdminIDs,
		logManager.Logger("telegram/adminui"),
		telegramAPI,
	)
	botService.SetAdminUI(adminAdapter)

	// Proactive participation: opt-in per chat through the interaction
	// profile. The engine evaluates eligible non-explicit group messages
	// only while the System One backend is configured; unconfigured keeps
	// the feature entirely off.
	var stopParticipation func()
	if systemOneClient.Configured() {
		stopParticipation = botService.SetProactiveDecisions(
			systemOneClient,
			decisionPersonaSource{prompts: prompts, fallbackChatModel: envChatRouteModel(cfg.LLM)},
			cfg.SystemOne.InputBudgetBytes,
			envChatRouteModel(cfg.LLM),
		)
	}

	attachImageTools(tools, botService, cfg.LLM.ImageRecognitionEnabled)
	// Confirmed non-text sends (polls, stickers, charts, ...) enter retained
	// history through the bot, which also applies reactions that arrived
	// while the message was still being sent.
	tools.AttachOutgoingRecorder(botService)
	// Delivered reminders are bot messages too; the scheduler is already
	// running, so the attachment is synchronized inside the service.
	reminderService.AttachDeliveryRecorder(botService)

	// Every tool is registered now (built-ins, MCP, decision and image
	// tools) and the catalog is static from here on: it lets the tool
	// statistics report registered tools with zero usage. Runs before the
	// bot starts handling updates.
	statsCollector.SetToolCatalog(tools.ToolNames())
	botService.SetChatRouteModel(envChatRouteModel(cfg.LLM))

	// Live previews subscribe to the execution events. At shutdown the
	// preview workers must be joined before the dispatcher drains and both
	// before the tool runtime closes (LIFO with the deferred tools.Close).
	stopPreviews := botService.SetExecutionDispatcher(dispatcher)
	// The admin UI watches MessageSent to re-create menus buried under newer
	// messages; its worker joins (unsubscribing first) before the preview
	// workers and the dispatcher drain. The worker is bound to the runtime
	// context so shutdown cancels a stalled menu send while handlers are
	// still being joined — a handler waiting on the render gate behind that
	// send cannot be interrupted itself, and would hold the handler join for
	// the whole re-creation send timeout.
	stopMenuRecreate := adminAdapter.StartWatching(ctx, dispatcher)

	runErr = botService.Run()
	// Proactive participation drains first: its tasks run full generation
	// turns that register preview runs, publish events, and record history,
	// so they must be joined while previews, the dispatcher, the tool
	// runtime, and the stores are all still alive.
	if stopParticipation != nil {
		stopParticipation()
	}
	stopMenuRecreate()
	if stopPreviews != nil {
		stopPreviews()
	}
	dispatcher.Stop()
	// Every statistics producer has been joined: flush what is pending.
	if err := statsPersister.Stop(); err != nil {
		logger.Error("final statistics flush failed; recently collected statistics may be lost", "error", err)
		sentry.CaptureException(err)
	}
	if runErr != nil {
		// Bot.Run already reports the underlying failure to Sentry on every
		// error path; a second generic event here carries no diagnostics.
		logger.Error("bot exited with error", "error", runErr)

		return runErr
	}

	return nil
}
