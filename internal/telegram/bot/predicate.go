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

// notAUserCommand matches commands that are not one of the user-facing
// commands. Combined with commandForMe it forms the unknown-command guard.
func (b *Bot) notAUserCommand() th.Predicate {
	return func(_ context.Context, update t.Update) bool {
		if update.Message == nil {
			return false
		}

		matches := th.CommandRegexp.FindStringSubmatch(update.Message.Text)
		if len(matches) != th.CommandMatchGroupsLen {
			return false
		}

		return !isUserCommand(matches[th.CommandMatchCmdGroup])
	}
}

// isUserCommand reports whether a command name (without slash) belongs to
// the supported user-facing command set. Every other command is treated as
// unknown and dropped before the conversation pipeline.
func isUserCommand(name string) bool {
	switch strings.ToLower(name) {
	case "start", "help", "summarize", "s", "stats", "reset":
		return true
	default:
		return false
	}
}
