package bot

import (
	"context"

	"telegram-ollama-reply-bot/internal/llm"
	"telegram-ollama-reply-bot/internal/state"

	t "github.com/mymmrac/telego"
)

func (b *Bot) createLlmRequestContextFromMessage(ctx context.Context, message t.Message) llm.RequestContext {
	rc := llm.RequestContext{
		Empty: true,
	}

	rc.Empty = false
	if ctx == nil {
		ctx = b.ctx
	}

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

	scope := scopeFromMessage(message)
	snapshot := b.getConversationSnapshot(scope)
	snapshot.Messages = b.hydrateMessagesWithImageDescriptions(ctx, snapshot.Messages)

	rc.Chat = llm.ChatContext{
		Title: chat.Title,
		// TODO: fill when ChatFullInfo retrieved
		//Description: chat.Description,
		Type:           chat.Type,
		History:        historyToLlmMessages(snapshot.Messages),
		EarlierSummary: snapshot.EarlierSummary,
	}

	b.loggerFromContext(ctx).Debug(
		"request context created",
		"history_messages", len(rc.Chat.History),
		"has_earlier_summary", rc.Chat.EarlierSummary != "",
		"chat_type", rc.Chat.Type,
	)

	return rc
}

func historyToLlmMessages(history []state.Message) []llm.ChatMessage {
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

func messageDataToLlmMessage(data state.Message) llm.ChatMessage {
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
