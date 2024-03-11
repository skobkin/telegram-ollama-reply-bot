package bot

import (
	"github.com/mymmrac/telego"
	"github.com/mymmrac/telego/telegohandler"
)

func (b *Bot) chatTypeStatsCounter(bot *telego.Bot, update telego.Update, next telegohandler.Handler) {
	message := update.Message

	if message == nil {
		next(bot, update)
	}

	switch message.Chat.Type {
	case telego.ChatTypeGroup, telego.ChatTypeSupergroup:
		b.stats.GroupRequest()
	case telego.ChatTypePrivate:
		b.stats.PrivateRequest()
		b.stats.PrivateRequest()
	}

	next(bot, update)
}
