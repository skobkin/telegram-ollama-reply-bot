package bot

import (
	"github.com/mymmrac/telego"
	tu "github.com/mymmrac/telego/telegoutil"
	"log/slog"
	"net/url"
	"slices"
	"strings"
)

var (
	allowedUrlSchemes = []string{"http", "https"}
)

func (b *Bot) reply(originalMessage *telego.Message, newMessage *telego.SendMessageParams) *telego.SendMessageParams {
	return newMessage.WithReplyParameters(&telego.ReplyParameters{
		MessageID: originalMessage.MessageID,
	})
}

func (b *Bot) sendTyping(chatId telego.ChatID) {
	slog.Debug("Setting 'typing' chat action")

	err := b.api.SendChatAction(tu.ChatAction(chatId, "typing"))
	if err != nil {
		slog.Error("Cannot set chat action", "error", err)
	}
}

func (b *Bot) trySendReplyError(message *telego.Message) {
	if message == nil {
		return
	}

	_, _ = b.api.SendMessage(b.reply(message, tu.Message(
		tu.ID(message.Chat.ID),
		"Error occurred while trying to send reply.",
	)))
}

func (b *Bot) trySendInlineQueryError(iq *telego.InlineQuery, text string) {
	if iq == nil {
		return
	}

	_ = b.api.AnswerInlineQuery(tu.InlineQuery(
		iq.ID,
		tu.ResultArticle(
			string("error_"+iq.ID),
			"Error: "+text,
			tu.TextMessage(text),
		),
	))
}

func isValidAndAllowedUrl(text string) bool {
	u, err := url.ParseRequestURI(text)
	if err != nil {
		slog.Debug("Provided text is not an URL", "text", text)

		return false
	}

	if !slices.Contains(allowedUrlSchemes, strings.ToLower(u.Scheme)) {
		slog.Debug("Provided URL has disallowed scheme", "scheme", u.Scheme, "allowed-schemes", allowedUrlSchemes)

		return false
	}

	return true
}
