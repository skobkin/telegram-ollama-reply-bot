package bot

import (
	"log/slog"

	"telegram-ollama-reply-bot/llm"

	t "github.com/mymmrac/telego"
)

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
	earlierSummary := ""
	if mh, ok := b.history[chat.ID]; ok {
		earlierSummary = mh.EarlierSummary()
	}

	rc.Chat = llm.ChatContext{
		Title: chat.Title,
		// TODO: fill when ChatFullInfo retrieved
		//Description: chat.Description,
		Type:           chat.Type,
		History:        historyToLlmMessages(history),
		EarlierSummary: earlierSummary,
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
