package bot

import (
	"errors"
	"log/slog"
	"net/url"
	"slices"
	"strings"

	"github.com/getsentry/sentry-go"
	"github.com/mymmrac/telego"
	th "github.com/mymmrac/telego/telegohandler"
	tu "github.com/mymmrac/telego/telegoutil"
)

var (
	allowedUrlSchemes = []string{"http", "https"}

	ErrImageRecognition = errors.New("image recognition error")
)

func (b *Bot) reply(originalMessage telego.Message, newMessage *telego.SendMessageParams) *telego.SendMessageParams {
	return newMessage.WithReplyParameters(&telego.ReplyParameters{
		MessageID: originalMessage.MessageID,
	})
}

func (b *Bot) sendTyping(chatId telego.ChatID) {
	slog.Debug("bot: Setting 'typing' chat action")

	err := b.api.SendChatAction(b.ctx, tu.ChatAction(chatId, "typing"))
	if err != nil {
		slog.Error("bot: Cannot set chat action", "error", err)
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
	textToCheck := message.Text
	if textToCheck == "" && message.Caption != "" {
		textToCheck = message.Caption
	}
	if textToCheck == "" {
		return false
	}

	slog.Debug("bot: Checking if message mentions me",
		"message_text", textToCheck,
		"bot_username", b.profile.Username,
		"contains_mention", strings.Contains(textToCheck, "@"+b.profile.Username))

	return strings.Contains(textToCheck, "@"+b.profile.Username)
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
		slog.Debug("bot: Provided text is not an URL", "text", text)
		sentry.CaptureException(err)

		return false
	}

	if !slices.Contains(allowedUrlSchemes, strings.ToLower(u.Scheme)) {
		slog.Debug("bot: Provided URL has disallowed scheme", "scheme", u.Scheme, "allowed-schemes", allowedUrlSchemes)

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

func (b *Bot) describeImage(photo telego.PhotoSize) (string, error) {
	file, err := b.api.GetFile(b.ctx, &telego.GetFileParams{
		FileID: photo.FileID,
	})
	if err != nil {
		slog.Error("bot: Failed to get file info", "error", err, "file_id", photo.FileID)
		return "", errors.Join(ErrImageRecognition, err)
	}

	fileBytes, err := tu.DownloadFile(b.api.FileDownloadURL(file.FilePath))
	if err != nil {
		slog.Error("bot: Failed to download file", "error", err, "file_path", file.FilePath)
		return "", errors.Join(ErrImageRecognition, err)
	}

	slog.Info("bot: Image downloaded", "file_path", file.FilePath, "file_size", len(fileBytes))

	description, err := b.llm.RecognizeImage(fileBytes)
	if err != nil {
		slog.Error("bot: Failed to recognize image", "error", err)
		return "", errors.Join(ErrImageRecognition, err)
	}

	slog.Debug("bot: Image recognized", "description", description)

	return description, nil
}

// gets MessageData from Telego request context if previously stored by history middleware, otherwise creates it on the fly
func (b *Bot) getMessageDataFromRequestContextOrCreate(ctx *th.Context, message telego.Message, isUserRequest bool) MessageData {
	if msgData, ok := ctx.Value(requestContextMessageDataKey).(MessageData); ok {
		msgData.IsUserRequest = isUserRequest
		slog.Debug("bot: Message data retrieved from context", "message_data", msgData)
		return msgData
	}

	msgData := b.tgUserMessageToMessageData(message, isUserRequest)
	slog.Debug("bot: Message data created from message on the fly", "message_data", msgData)
	return msgData
}
