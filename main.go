package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	openai "github.com/sashabaranov/go-openai"

	tg "github.com/mymmrac/telego"
	tu "github.com/mymmrac/telego/telegoutil"
)

const (
	ModelMistral           = "mistral"
	ModelMistralUncensored = "dolphin-mistral"
)

func main() {
	ollamaToken := os.Getenv("OLLAMA_TOKEN")
	ollamaBaseUrl := os.Getenv("OLLAMA_BASE_URL")

	telegramToken := os.Getenv("TELEGRAM_TOKEN")

	config := openai.DefaultConfig(ollamaToken)
	config.BaseURL = ollamaBaseUrl

	client := openai.NewClientWithConfig(config)

	bot, err := tg.NewBot(telegramToken, tg.WithDefaultLogger(false, true))
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}

	// Get updates channel
	updates, _ := bot.UpdatesViaLongPolling(nil)

	// Stop reviving updates from update channel
	defer bot.StopLongPolling()

	// Loop through all updates when they came
	for update := range updates {
		// Check if update contains a message
		if update.Message != nil {
			slog.Info("Update with message received", update.Message.Chat, update.Message.From, update.Message.Text)

			chatID := tu.ID(update.Message.Chat.ID)

			req := openai.ChatCompletionRequest{
				Model: ModelMistralUncensored,
				Messages: []openai.ChatCompletionMessage{
					{
						Role:    openai.ChatMessageRoleSystem,
						Content: "You're a bot in the Telegram chat. You are replying to questions directed to you.",
					},
				},
			}

			req.Messages = append(req.Messages, openai.ChatCompletionMessage{
				Role:    openai.ChatMessageRoleUser,
				Content: update.Message.Text,
			})
			resp, err := client.CreateChatCompletion(context.Background(), req)
			if err != nil {
				slog.Error("ChatCompletion error", err)

				continue
			}

			slog.Info("Got completion. Going to send.", resp.Choices[0])

			message := tu.Message(
				chatID,
				resp.Choices[0].Message.Content,
			)
			message = message.WithReplyParameters(&tg.ReplyParameters{MessageID: update.Message.MessageID})
			message = message.WithParseMode("Markdown")

			_, err = bot.SendMessage(message)

			if err != nil {
				slog.Error("Can't send reply message", err)
			}

			//req.Messages = append(req.Messages, resp.Choices[0].Message)
		}
	}
}
