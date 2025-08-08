package bot

import (
	"log/slog"
	"strings"

	"github.com/getsentry/sentry-go"
	"telegram-ollama-reply-bot/llm"

	t "github.com/mymmrac/telego"
)

const maxHistoryMessages = 15

func (b *Bot) createLlmRequestContextFromMessage(message t.Message) llm.RequestContext {
	rc := llm.RequestContext{
		Empty: true,
	}

	rc.Empty = false

	user := message.From

	if user != nil {
		rc.User = llm.UserContext{
			Username:  user.Username,
			FirstName: user.FirstName,
			LastName:  user.LastName,
			IsPremium: user.IsPremium,
		}
	}

	// TODO: implement retrieval of chat description
	chat := message.Chat

	history := b.getChatHistory(chat.ID)
	var summary string
	if len(history) > maxHistoryMessages {
		earlier := history[:len(history)-maxHistoryMessages]
		history = history[len(history)-maxHistoryMessages:]

		text := historyToPlainText(earlier)
		if text != "" {
			var err error
			summary, err = b.llm.Summarize(text, "")
			if err != nil {
				slog.Error("bot: failed to summarize earlier messages", "error", err)
				sentry.CaptureException(err)
			}
		}
	}

	rc.Chat = llm.ChatContext{
		Title: chat.Title,
		// TODO: fill when ChatFullInfo retrieved
		//Description: chat.Description,
		Type:           chat.Type,
		History:        historyToLlmMessages(history),
		EarlierSummary: summary,
	}

	slog.Debug("bot: request context created", "request-context", rc)

	return rc
}

func historyToLlmMessages(history []MessageData) []llm.ChatMessage {
	length := len(history)

	if length > 0 {
		result := make([]llm.ChatMessage, 0, length)

		for _, msg := range history {
			result = append(result, messageDataToLlmMessage(msg))
		}

		return result
	}

	return make([]llm.ChatMessage, 0)
}

func messageDataToLlmMessage(data MessageData) llm.ChatMessage {
	llmMessage := llm.ChatMessage{
		Name:          data.Name,
		Username:      data.Username,
		Text:          data.Text,
		IsMe:          data.IsMe,
		IsUserRequest: data.IsUserRequest,
		HasImage:      data.HasImage,
		Image:         data.Image,
	}

	if data.ReplyTo != nil {
		replyMessage := messageDataToLlmMessage(*data.ReplyTo)
		llmMessage.ReplyTo = &replyMessage
	}

	return llmMessage
}

func historyToPlainText(history []MessageData) string {
	var sb strings.Builder
	for _, msg := range history {
		sb.WriteString(msg.Name)
		if msg.Username != "" {
			sb.WriteString(" (@")
			sb.WriteString(msg.Username)
			sb.WriteString(")")
		}
		sb.WriteString(": ")
		sb.WriteString(msg.Text)
		sb.WriteString("\n")
	}
	return sb.String()
}
