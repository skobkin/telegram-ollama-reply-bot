package app

import (
	"context"
	"fmt"
	"telegram-ollama-reply-bot/internal/adminconfig"
	"telegram-ollama-reply-bot/internal/config"
	"telegram-ollama-reply-bot/internal/content/extractor"
	"telegram-ollama-reply-bot/internal/content/search"
	"telegram-ollama-reply-bot/internal/llm"
	"telegram-ollama-reply-bot/internal/logging"
	psqlite "telegram-ollama-reply-bot/internal/persistence/sqlite"
	"telegram-ollama-reply-bot/internal/reminders"
	"telegram-ollama-reply-bot/internal/state/memory"
	"telegram-ollama-reply-bot/internal/support/httpclient"
	"telegram-ollama-reply-bot/internal/support/markdown"
	"telegram-ollama-reply-bot/internal/telegram/bot"
	"telegram-ollama-reply-bot/internal/telegram/transport"
	"telegram-ollama-reply-bot/internal/tooluse"
	"time"

	"github.com/getsentry/sentry-go"
	tg "github.com/mymmrac/telego"
)

func Run(ctx context.Context) error {
	cfg := config.Load()
	logManager, err := logging.NewManager(logging.Options{
		Level:      cfg.Logging.Level,
		SetDefault: true,
	})
	if err != nil {
		return fmt.Errorf("configure logging: %w", err)
	}
	logger := logManager.Logger("app")

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
		"image_model", cfg.LLM.Features.ImageRecognition.Model,
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

	llmc, err := llm.NewService(cfg.LLM, llm.NewAdminPromptRenderer(adminService), logManager.Logger("llm"))
	if err != nil {
		logger.Error("failed to initialize llm service", "error", err)
		sentry.CaptureException(err)

		return err
	}

	logger.Debug("checking models availability")

	hasAll, searchResult := llmc.HasAllModels(ctx)
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
	botService := bot.NewBot(
		ctx,
		telegramAPI,
		llmc,
		ext,
		sanitizer,
		stores.Conversations(),
		stores.Images(),
		stores.Stats(),
		cfg.Bot,
		adminService,
		tools,
		logManager.Logger("telegram/bot"),
	)

	if err := botService.Run(); err != nil {
		logger.Error("bot exited with error", "error", err)
		sentry.CaptureMessage("Bot start error")

		return err
	}

	return nil
}
