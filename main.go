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
	ollamaToken := os.Getenv("OLLAMA_TOKEN")
	ollamaBaseUrl := os.Getenv("OLLAMA_BASE_URL")

	telegramToken := os.Getenv("TELEGRAM_TOKEN")

	llmc := llm.NewConnector(ollamaBaseUrl, ollamaToken)
	ext := extractor.NewExtractor()

	telegramApi, err := tg.NewBot(telegramToken, tg.WithLogger(bot.NewLogger("telego: ")))
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}

	botService := bot.NewBot(telegramApi, llmc, ext)

	err = botService.Run()
	if err != nil {
		slog.Error("Running bot finished with an error", "error", err)
		os.Exit(1)
	}
}
