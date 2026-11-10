package app

import (
	"context"
	"fmt"
	"telegram-ollama-reply-bot/internal/adminconfig"
	"telegram-ollama-reply-bot/internal/buildinfo"
	"telegram-ollama-reply-bot/internal/config"
	"telegram-ollama-reply-bot/internal/content/extractor"
	"telegram-ollama-reply-bot/internal/content/search"
	"telegram-ollama-reply-bot/internal/llm"
	"telegram-ollama-reply-bot/internal/logging"
	"telegram-ollama-reply-bot/internal/mcp"
	psqlite "telegram-ollama-reply-bot/internal/persistence/sqlite"
	"telegram-ollama-reply-bot/internal/reminders"
	"telegram-ollama-reply-bot/internal/state/memory"
	"telegram-ollama-reply-bot/internal/support/httpclient"
	"telegram-ollama-reply-bot/internal/support/markdown"
	"telegram-ollama-reply-bot/internal/telegram/bot"
	"telegram-ollama-reply-bot/internal/telegram/transport"
	"telegram-ollama-reply-bot/internal/tooluse"
	"telegram-ollama-reply-bot/internal/vision"
	"time"

	"github.com/getsentry/sentry-go"
	tg "github.com/mymmrac/telego"
)

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

// imageModelLogValue returns the value logged as image_model: the configured
// model on an OpenAI-compatible route, or an empty string when the active
// recognizer does not use one (the recognizer name is logged separately).
func imageModelLogValue(cfg *config.Config) string {
	if !cfg.LLM.ImageRecognitionEnabled || cfg.LLM.ImageRecognition.Recognizer != config.ImageRecognizerOpenAICompat {
		return ""
	}

	return cfg.LLM.Features.ImageRecognition.Model
}

func Run(ctx context.Context) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	logManager, err := logging.NewManager(logging.Options{
		Level:      cfg.Logging.Level,
		SetDefault: true,
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

	persistentStore, err := psqlite.Open(ctx, cfg.Persistence.StorePath, logManager.Logger("persistence/sqlite"))
	if err != nil {
		logger.Error("failed to initialize persistent store", "error", err)
		sentry.CaptureException(err)

		return err
	}
	defer func() { _ = persistentStore.Close() }()

	adminService := adminconfig.NewService(persistentStore)

	prompts := llm.NewAdminPromptRenderer(adminService, cfg.LLM.ImageRecognitionEnabled)

	llmc, err := llm.NewService(cfg.LLM, prompts, logManager.Logger("llm"))
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

		imageLLM, err = llm.NewService(imageCfg, prompts, logManager.Logger("llm/image"))
		if err != nil {
			logger.Error("failed to initialize image llm service", "error", err)
			sentry.CaptureException(err)

			return err
		}

		recognizer = vision.NewOpenAIRecognizer(imageLLM)
	default:
		recognizer = vision.NewOpenAIRecognizer(llmc)
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

	ext := extractor.NewExtractor(logManager.Logger("content/extractor"))
	searcher, err := search.NewFromConfig(*cfg, httpclient.New(), logManager.Logger("content/search"))
	if err != nil {
		logger.Error("failed to initialize search capability", "error", err)
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

	telegramAPI, err := tg.NewBot(cfg.Bot.Telegram.Token, tg.WithLogger(bot.NewLogger(
		logManager.Logger("telegram/bot").With("source", "telego"),
		cfg.Bot.Telegram.Token,
	)), tg.WithHTTPClient(httpclient.New()))
	if err != nil {
		logger.Error("telegram api initialization failed", "error", err)
		sentry.CaptureException(err)

		return err
	}

	sanitizer := markdown.NewTgMarkdownV2Sanitizer()
	reminderService := reminders.NewService(persistentStore, telegramAPI, cfg.Bot.AdminIDs, logManager.Logger("reminders"))
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
		telegramAPI,
		reminderService,
		logManager.Logger("tooluse"),
		tooluse.Config{MaxIterations: cfg.LLM.ToolLoopMaxIterations, AdminIDs: cfg.Bot.AdminIDs, RecentHistoryLimit: cfg.Bot.UncompressedHistoryLimit},
	)
	tools.AttachMCP("mcp", mcpManager)
	defer tools.Close()
	botService := bot.NewBot(
		ctx,
		telegramAPI,
		llmc,
		visionService,
		ext,
		sanitizer,
		stores.Conversations(),
		stores.Images(),
		stores.Stats(),
		cfg.Bot,
		adminService,
		tools,
		logManager.Logger("telegram/bot"),
		bot.ImageRecognitionSettings{
			Enabled:       cfg.LLM.ImageRecognitionEnabled,
			MaxImageBytes: cfg.LLM.ImageRecognition.MaxImageBytes,
		},
	)

	if err := botService.Run(); err != nil {
		logger.Error("bot exited with error", "error", err)
		sentry.CaptureMessage("Bot start error")

		return err
	}

	return nil
}
