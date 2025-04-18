package bot

import (
	"log/slog"

	t "github.com/mymmrac/telego"
	th "github.com/mymmrac/telego/telegohandler"
)

// requestContextMessageDataKey is the context key for storing processed message data in request context
const requestContextMessageDataKey = "message_data"

func (b *Bot) chatTypeStatsCounter(ctx *th.Context, update t.Update) error {
	message := update.Message

	if message == nil {
		slog.Info("bot:middleware:stats: update has no message. skipping.")
		return ctx.Next(update)
	}

	switch message.Chat.Type {
	case t.ChatTypeGroup, t.ChatTypeSupergroup:
		if b.isMentionOfMe(*message) || b.isReplyToMe(*message) {
			slog.Info("bot:middleware:stats: counting message chat type in stats", "type", message.Chat.Type)
			b.stats.GroupRequest()
		}
	case t.ChatTypePrivate:
		slog.Info("bot:middleware:stats: counting message chat type in stats", "type", message.Chat.Type)
		b.stats.PrivateRequest()
	}
	return ctx.Next(update)
}

func (b *Bot) chatHistory(ctx *th.Context, update t.Update) error {
	message := update.Message

	if message == nil {
		slog.Info("bot:middleware:history: update has no message. skipping.")
		return ctx.Next(update)
	}

	slog.Debug("bot:middleware:history: saving message to history for", "chat_id", message.Chat.ID)

	slog.Info(
		"bot:middleware:history: saving message",
		"chat", message.Chat.ID,
		"chat_type", message.Chat.Type,
		"chat_name", message.Chat.Title,
		"from_id", message.From.ID,
		"from_name", message.From.FirstName,
		"has_image", len(message.Photo) > 0,
		"caption", message.Caption,
		"text", message.Text,
	)

	// Process message and store in context
	msgData := b.tgUserMessageToMessageData(*message, false)
	ctx = ctx.WithValue(requestContextMessageDataKey, msgData)

	// Save to history
	b.saveChatMessageToHistory(msgData)

	return ctx.Next(update)
}
