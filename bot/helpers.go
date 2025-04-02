package bot

import (
	"log/slog"
	"net/url"
	"slices"
	"strings"

	"github.com/getsentry/sentry-go"
	"github.com/mymmrac/telego"
	tu "github.com/mymmrac/telego/telegoutil"
)

var (
	allowedUrlSchemes = []string{"http", "https"}
)

func (b *Bot) reply(originalMessage telego.Message, newMessage *telego.SendMessageParams) *telego.SendMessageParams {
	return newMessage.WithReplyParameters(&telego.ReplyParameters{
		MessageID: originalMessage.MessageID,
	})
}

func (b *Bot) sendTyping(chatId telego.ChatID) {
	slog.Debug("Setting 'typing' chat action")

	err := b.api.SendChatAction(b.ctx, tu.ChatAction(chatId, "typing"))
	if err != nil {
		slog.Error("Cannot set chat action", "error", err)
		sentry.CaptureException(err)
	}
}

func (b *Bot) trySendReplyError(message telego.Message) {
	_, _ = b.api.SendMessage(b.ctx, b.reply(message, tu.Message(
		tu.ID(message.Chat.ID),
		"Error occurred while trying to send reply.",
	)))
}

func (b *Bot) isMentionOfMe(message telego.Message) bool {
	if message.Text == "" {
		return false
	}

	slog.Debug("bot: Checking if message mentions me",
		"message_text", message.Text,
		"bot_username", b.profile.Username,
		"contains_mention", strings.Contains(message.Text, "@"+b.profile.Username))

	return strings.Contains(message.Text, "@"+b.profile.Username)
}

func (b *Bot) isReplyToMe(message telego.Message) bool {
	if message.ReplyToMessage == nil {
		return false
	}
	if message.ReplyToMessage.From == nil {
		return false
	}

	replyToMessage := message.ReplyToMessage

	return replyToMessage != nil && replyToMessage.From.ID == b.profile.Id
}

func (b *Bot) isPrivateWithMe(message telego.Message) bool {
	return message.Chat.Type == telego.ChatTypePrivate
}

func isValidAndAllowedUrl(text string) bool {
	u, err := url.ParseRequestURI(text)
	if err != nil {
		slog.Debug("Provided text is not an URL", "text", text)
		sentry.CaptureException(err)

		return false
	}

	if !slices.Contains(allowedUrlSchemes, strings.ToLower(u.Scheme)) {
		slog.Debug("Provided URL has disallowed scheme", "scheme", u.Scheme, "allowed-schemes", allowedUrlSchemes)

		return false
	}

	return true
}

func cropToMaxLengthMarkdownV2(text string, max int) string {
	if len(text) <= max {
		return text
	}

	cropPoint := max - 3
	for cropPoint > 0 && text[cropPoint] != ' ' {
		cropPoint--
	}

	return text[:cropPoint] + "\\.\\.\\."
}

func (b *Bot) isFromAdmin(message *telego.Message) bool {
	if message == nil || message.From == nil {
		return false
	}

	return slices.Contains(b.cfg.AdminIDs, message.From.ID)
}

func (b *Bot) escapeMarkdownV1Symbols(input string) string {
	return b.markdownV1Replacer.Replace(input)
}

func (b *Bot) escapeMarkdownV2Symbols(input string) string {
	specialChars := "_*[]()~`>#+-=|{}.!"
	var escaped strings.Builder

	for _, char := range input {
		if strings.ContainsRune(specialChars, char) {
			escaped.WriteRune('\\')
		}
		escaped.WriteRune(char)
	}

	return escaped.String()
}
