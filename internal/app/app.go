package app

import (
	"context"
	"fmt"
	"log/slog"
	"telegram-ollama-reply-bot/internal/config"
	"telegram-ollama-reply-bot/internal/content/extractor"
	"telegram-ollama-reply-bot/internal/llm"
	"telegram-ollama-reply-bot/internal/support/markdown"
	"telegram-ollama-reply-bot/internal/telegram/bot"
	"time"

	"github.com/getsentry/sentry-go"
	tg "github.com/mymmrac/telego"
)

func Run(ctx context.Context) error {
	cfg := config.Load()

	if cfg.Sentry.DSN != "" {
		slog.Info("app: Initializing sentry with provided DSN")

		err := sentry.Init(sentry.ClientOptions{
			Dsn:              cfg.Sentry.DSN,
			AttachStacktrace: true,
		})
		if err != nil {
			slog.Error("app: Sentry initialization failed", "error", err)
		} else {
			defer sentry.Flush(2 * time.Second)
		}
	} else {
		slog.Info("app: Sentry disabled (no DSN provided)")
	}

	slog.Info("app: Selected", "models", cfg.LLM.Models)

	templateProcessor, err := llm.NewTemplateProcessor(cfg.LLM.Prompts)
	if err != nil {
		slog.Error("app: Failed to initialize template processor", "error", err)
		sentry.CaptureException(err)

		return err
	}

	llmc := llm.NewConnector(cfg.LLM, templateProcessor)

	slog.Info("app: Checking models availability")

	hasAll, searchResult := llmc.HasAllModels(ctx, cfg.LLM.Models)
	if !hasAll {
		slog.Error("app: Not all models are available", "result", searchResult)
		sentry.CaptureMessage("Not all models are available")

		return fmt.Errorf("missing required models: %v", searchResult)
	}

	slog.Info("app: All needed models are available")

	ext := extractor.NewExtractor()

	telegramAPI, err := tg.NewBot(cfg.Bot.Telegram.Token, tg.WithLogger(bot.NewLogger("telego: ")))
	if err != nil {
		slog.Error("app: Telegram API initialization failed", "error", err)
		sentry.CaptureException(err)

		return err
	}

	sanitizer := markdown.NewTgMarkdownV2Sanitizer()
	botService := bot.NewBot(ctx, telegramAPI, llmc, ext, sanitizer, bot.NewImageCache(), cfg.Bot)

	if err := botService.Run(); err != nil {
		slog.Error("app: Running bot finished with an error", "error", err)
		sentry.CaptureMessage("Bot start error")

		return err
	}

	return nil
}
