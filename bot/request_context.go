package bot

import (
	"github.com/mymmrac/telego"
	"log/slog"
	"telegram-ollama-reply-bot/llm"
)

func (b *Bot) createLlmRequestContext(update telego.Update) llm.RequestContext {
	message := update.Message
	iq := update.InlineQuery

	rc := llm.RequestContext{
		Empty:  true,
		Inline: false,
	}

	switch {
	case message == nil && iq == nil:
		slog.Debug("request context creation problem: no message provided. returning empty context.", "request-context", rc)

		return rc
	case iq != nil:
		rc.Inline = true
	}

	rc.Empty = false

	var user *telego.User

	if rc.Inline {
		user = &iq.From
	} else {
		user = message.From
	}

	if user != nil {
		rc.User = llm.UserContext{
			Username:  user.Username,
			FirstName: user.FirstName,
			LastName:  user.LastName,
			IsPremium: user.IsPremium,
		}
	}

	if !rc.Inline {
		chat := message.Chat
		rc.Chat = llm.ChatContext{
			Title:       chat.Title,
			Description: chat.Description,
			Type:        chat.Type,
		}
	}

	slog.Debug("request context created", "request-context", rc)

	return rc
}
