package bot

import (
	"context"
	"strings"

	t "github.com/mymmrac/telego"
	th "github.com/mymmrac/telego/telegohandler"
)

func (b *Bot) commandForThisBot() th.Predicate {
	return func(_ context.Context, update t.Update) bool {
		if update.Message == nil {
			return false
		}

		matches := th.CommandRegexp.FindStringSubmatch(update.Message.Text)
		if len(matches) != th.CommandMatchGroupsLen {
			return false
		}

		addressedUsername := matches[th.CommandMatchBotUsernameGroup]
		if addressedUsername == "" {
			return true
		}

		if b.me.Username == "" {
			return false
		}

		return strings.EqualFold(addressedUsername, b.me.Username)
	}
}
