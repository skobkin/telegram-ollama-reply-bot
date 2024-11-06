package main

import (
	"fmt"
	"log/slog"
	"os"
	"telegram-ollama-reply-bot/bot"
	"telegram-ollama-reply-bot/extractor"
	"telegram-ollama-reply-bot/llm"

	tg "github.com/mymmrac/telego"
)

func main() {
	apiToken := os.Getenv("OPENAI_API_TOKEN")
	apiBaseUrl := os.Getenv("OPENAI_API_BASE_URL")

	models := bot.ModelSelection{
		TextRequestModel: os.Getenv("MODEL_TEXT_REQUEST"),
		SummarizeModel:   os.Getenv("MODEL_SUMMARIZE_REQUEST"),
	}

	slog.Info("Selected", "models", models)

	telegramToken := os.Getenv("TELEGRAM_TOKEN")

	llmc := llm.NewConnector(apiBaseUrl, apiToken)

	slog.Info("Checking models availability")

	hasAll, searchResult := llmc.HasAllModels([]string{models.TextRequestModel, models.SummarizeModel})
	if !hasAll {
		slog.Error("Not all models are available", "result", searchResult)
		os.Exit(1)
	}

	slog.Info("All needed models are available")

	ext := extractor.NewExtractor()

	telegramApi, err := tg.NewBot(telegramToken, tg.WithLogger(bot.NewLogger("telego: ")))
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}

	botService := bot.NewBot(telegramApi, llmc, ext, models)

	err = botService.Run()
	if err != nil {
		slog.Error("Running bot finished with an error", "error", err)
		os.Exit(1)
	}
}
