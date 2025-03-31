package main

import (
	"fmt"
	"log/slog"
	"os"
	"telegram-ollama-reply-bot/bot"
	"telegram-ollama-reply-bot/config"
	"telegram-ollama-reply-bot/extractor"
	"telegram-ollama-reply-bot/llm"
	"time"

	"github.com/getsentry/sentry-go"

	tg "github.com/mymmrac/telego"
)

func main() {
	cfg := config.Load()

	if cfg.Sentry.DSN != "" {
		slog.Info("main: Initializing sentry with provided DSN")

		if err := sentry.Init(sentry.ClientOptions{
			Dsn:              cfg.Sentry.DSN,
			AttachStacktrace: true,
		}); err != nil {
			slog.Error("main: Sentry initialization failed", "error", err)
		}
	} else {
		slog.Info("main: Sentry disabled (no DSN provided)")
	}

	defer sentry.Flush(2 * time.Second)

	slog.Info("main: Selected", "models", cfg.Bot.Models)

	llmc := llm.NewConnector(cfg.LLM)

	slog.Info("main: Checking models availability")

	hasAll, searchResult := llmc.HasAllModels(cfg.Bot.Models)
	if !hasAll {
		slog.Error("main: Not all models are available", "result", searchResult)
		sentry.CaptureMessage("Not all models are available")

		os.Exit(1)
	}

	slog.Info("main: All needed models are available")

	ext := extractor.NewExtractor()

	telegramApi, err := tg.NewBot(cfg.Telegram.Token, tg.WithLogger(bot.NewLogger("telego: ")))
	if err != nil {
		fmt.Println(err)
		sentry.CaptureMessage("Telegram API initialization failed")

		os.Exit(1)
	}

	botService := bot.NewBot(telegramApi, llmc, ext, cfg.Bot)

	err = botService.Run()
	if err != nil {
		slog.Error("main: Running bot finished with an error", "error", err)
		sentry.CaptureMessage("Bot start error")

		os.Exit(1)
	}
}
