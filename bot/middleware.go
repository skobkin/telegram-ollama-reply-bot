package bot

import (
	"github.com/mymmrac/telego"
	"github.com/mymmrac/telego/telegohandler"
	"log/slog"
)

func (b *Bot) chatTypeStatsCounter(bot *telego.Bot, update telego.Update, next telegohandler.Handler) {
	message := update.Message

	if message == nil {
		slog.Info("chat-type-middleware: update has no message. skipping.")

		next(bot, update)

		return
	}

	slog.Info("chat-type-middleware: counting message chat type in stats", "type", message.Chat.Type)

	switch message.Chat.Type {
	case telego.ChatTypeGroup, telego.ChatTypeSupergroup:
		b.stats.GroupRequest()
	case telego.ChatTypePrivate:
		b.stats.PrivateRequest()
	}

	next(bot, update)
}
