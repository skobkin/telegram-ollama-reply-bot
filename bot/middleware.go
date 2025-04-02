package bot

import (
	"log/slog"

	"github.com/mymmrac/telego"
	th "github.com/mymmrac/telego/telegohandler"
)

func (b *Bot) chatTypeStatsCounter(ctx *th.Context, update telego.Update) error {
	message := update.Message

	if message == nil {
		slog.Info("stats-middleware: update has no message. skipping.")
		return ctx.Next(update)
	}

	switch message.Chat.Type {
	case telego.ChatTypeGroup, telego.ChatTypeSupergroup:
		if b.isMentionOfMe(*message) || b.isReplyToMe(*message) {
			slog.Info("stats-middleware: counting message chat type in stats", "type", message.Chat.Type)
			b.stats.GroupRequest()
		}
	case telego.ChatTypePrivate:
		slog.Info("stats-middleware: counting message chat type in stats", "type", message.Chat.Type)
		b.stats.PrivateRequest()
	}
	return ctx.Next(update)
}

func (b *Bot) chatHistory(ctx *th.Context, update telego.Update) error {
	message := update.Message

	if message == nil {
		slog.Info("chat-history-middleware: update has no message. skipping.")
		return ctx.Next(update)
	}

	slog.Debug("chat-history-middleware: saving message to history for", "chat_id", message.Chat.ID)

	b.saveChatMessageToHistory(*message)
	return ctx.Next(update)
}
