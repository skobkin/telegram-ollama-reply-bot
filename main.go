package main

import (
	"fmt"
	"log/slog"
	"os"
	"telegram-ollama-reply-bot/bot"
	"telegram-ollama-reply-bot/extractor"
	"telegram-ollama-reply-bot/llm"
	"time"

	"github.com/getsentry/sentry-go"

	tg "github.com/mymmrac/telego"
)

func main() {
	apiToken := os.Getenv("OPENAI_API_TOKEN")
	apiBaseUrl := os.Getenv("OPENAI_API_BASE_URL")
	sentryDsn := os.Getenv("SENTRY_DSN")

	if sentryDsn != "" {
		slog.Info("main: Initializing sentry with provided DSN")

		if err := sentry.Init(sentry.ClientOptions{
			Dsn:              sentryDsn,
			AttachStacktrace: true,
		}); err != nil {
			slog.Error("main: Sentry initialization failed", "error", err)
		}
	} else {
		slog.Info("main: Sentry disabled (no DSN provided)")
	}

	defer sentry.Flush(2 * time.Second)

	models := bot.ModelSelection{
		TextRequestModel: os.Getenv("MODEL_TEXT_REQUEST"),
		SummarizeModel:   os.Getenv("MODEL_SUMMARIZE_REQUEST"),
	}

	slog.Info("main: Selected", "models", models)

	telegramToken := os.Getenv("TELEGRAM_TOKEN")

	llmc := llm.NewConnector(apiBaseUrl, apiToken)

	slog.Info("main: Checking models availability")

	hasAll, searchResult := llmc.HasAllModels([]string{models.TextRequestModel, models.SummarizeModel})
	if !hasAll {
		slog.Error("main: Not all models are available", "result", searchResult)
		sentry.CaptureMessage("Not all models are available")

		os.Exit(1)
	}

	slog.Info("main: All needed models are available")

	ext := extractor.NewExtractor()

	telegramApi, err := tg.NewBot(telegramToken, tg.WithLogger(bot.NewLogger("telego: ")))
	if err != nil {
		fmt.Println(err)
		sentry.CaptureMessage("Telegram API initialization failed")

		os.Exit(1)
	}

	botService := bot.NewBot(telegramApi, llmc, ext, models)

	err = botService.Run()
	if err != nil {
		slog.Error("main: Running bot finished with an error", "error", err)
		sentry.CaptureMessage("Bot start error")

		os.Exit(1)
	}
}
