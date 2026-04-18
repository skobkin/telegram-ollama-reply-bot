package app

import (
	"context"
	"fmt"
	"telegram-ollama-reply-bot/internal/config"
	"telegram-ollama-reply-bot/internal/content/extractor"
	"telegram-ollama-reply-bot/internal/llm"
	"telegram-ollama-reply-bot/internal/logging"
	"telegram-ollama-reply-bot/internal/support/markdown"
	"telegram-ollama-reply-bot/internal/telegram/bot"
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
		logger.Info("initializing sentry")

		err = sentry.Init(sentry.ClientOptions{
			Dsn:              cfg.Sentry.DSN,
			AttachStacktrace: true,
		})
		if err != nil {
			logger.Error("sentry initialization failed", "error", err)
		} else {
			defer sentry.Flush(2 * time.Second)
		}
	} else {
		logger.Info("sentry disabled")
	}

	logger.Info(
		"selected models",
		"text_request_model", cfg.LLM.Models.TextRequestModel,
		"summarize_model", cfg.LLM.Models.SummarizeModel,
		"image_recognition_model", cfg.LLM.Models.ImageRecognitionModel,
	)

	templateProcessor, err := llm.NewTemplateProcessor(cfg.LLM.Prompts)
	if err != nil {
		logger.Error("failed to initialize template processor", "error", err)
		sentry.CaptureException(err)

		return err
	}

	llmc := llm.NewConnector(cfg.LLM, templateProcessor, logManager.Logger("llm"))

	logger.Info("checking models availability")

	hasAll, searchResult := llmc.HasAllModels(ctx, cfg.LLM.Models)
	if !hasAll {
		logger.Error("required models are unavailable", "result", searchResult)
		sentry.CaptureMessage("Not all models are available")

		return fmt.Errorf("missing required models: %v", searchResult)
	}

	logger.Info("all required models are available")

	ext := extractor.NewExtractor(logManager.Logger("content/extractor"))

	telegramAPI, err := tg.NewBot(cfg.Bot.Telegram.Token, tg.WithLogger(bot.NewLogger(
		logManager.Logger("telegram/bot").With("source", "telego"),
		cfg.Bot.Telegram.Token,
	)))
	if err != nil {
		logger.Error("telegram api initialization failed", "error", err)
		sentry.CaptureException(err)

		return err
	}

	sanitizer := markdown.NewTgMarkdownV2Sanitizer()
	botService := bot.NewBot(ctx, telegramAPI, llmc, ext, sanitizer, bot.NewImageCache(), cfg.Bot, logManager.Logger("telegram/bot"))

	if err := botService.Run(); err != nil {
		logger.Error("bot exited with error", "error", err)
		sentry.CaptureMessage("Bot start error")

		return err
	}

	return nil
}
