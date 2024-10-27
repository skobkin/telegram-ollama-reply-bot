package bot

import (
	"github.com/mymmrac/telego"
	"github.com/mymmrac/telego/telegohandler"
	"log/slog"
)

func (b *Bot) chatTypeStatsCounter(bot *telego.Bot, update telego.Update, next telegohandler.Handler) {
	message := update.Message

	if message == nil {
		slog.Info("stats-middleware: update has no message. skipping.")

		next(bot, update)

		return
	}

	switch message.Chat.Type {
	case telego.ChatTypeGroup, telego.ChatTypeSupergroup:
		if b.isMentionOfMe(update) || b.isReplyToMe(update) {
			slog.Info("stats-middleware: counting message chat type in stats", "type", message.Chat.Type)
			b.stats.GroupRequest()
		}
	case telego.ChatTypePrivate:
		slog.Info("stats-middleware: counting message chat type in stats", "type", message.Chat.Type)
		b.stats.PrivateRequest()
	}

	next(bot, update)
}

func (b *Bot) chatHistory(bot *telego.Bot, update telego.Update, next telegohandler.Handler) {
	message := update.Message

	if message == nil {
		slog.Info("chat-history-middleware: update has no message. skipping.")

		next(bot, update)

		return
	}

	slog.Info("chat-history-middleware: saving message to history for", "chat_id", message.Chat.ID)

	b.saveChatMessageToHistory(message)

	next(bot, update)
}
