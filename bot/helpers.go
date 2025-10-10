package bot

import (
	"context"
	"errors"
	"log/slog"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/getsentry/sentry-go"
	t "github.com/mymmrac/telego"
	th "github.com/mymmrac/telego/telegohandler"
	tu "github.com/mymmrac/telego/telegoutil"
)

var (
	allowedUrlSchemes = []string{"http", "https"}

	ErrImageRecognition = errors.New("image recognition error")
)

func (b *Bot) reply(originalMessage t.Message, newMessage *t.SendMessageParams) *t.SendMessageParams {
	return newMessage.WithReplyParameters(&t.ReplyParameters{
		MessageID: originalMessage.MessageID,
	})
}

func (b *Bot) sendTyping(chatId t.ChatID) {
	slog.Debug("bot: Setting 'typing' chat action")

	err := b.api.SendChatAction(b.ctx, tu.ChatAction(chatId, "typing"))
	if err != nil {
		slog.Error("bot: Cannot set chat action", "error", err)
		sentry.CaptureException(err)
	}
}

func (b *Bot) sendTypingUntil(ctx context.Context, chatId t.ChatID) {
	b.sendTyping(chatId)
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			b.sendTyping(chatId)
		}
	}
}

func (b *Bot) trySendReplyError(message t.Message) {
	_, _ = b.api.SendMessage(b.ctx, b.reply(message, tu.Message(
		tu.ID(message.Chat.ID),
		"Error occurred while trying to send reply.",
	)))
}

func (b *Bot) isMentionOfMe(message t.Message) bool {
	textToCheck := message.Text
	entities := message.Entities
	if textToCheck == "" && message.Caption != "" {
		textToCheck = message.Caption
		entities = message.CaptionEntities
	}
	if textToCheck == "" {
		return false
	}

	for _, e := range entities {
		switch e.Type {
		case t.EntityTypeTextMention:
			if e.User != nil && e.User.ID == b.me.ID {
				return true
			}
		case t.EntityTypeMention:
			if entityText(textToCheck, e) == "@"+b.me.Username {
				return true
			}
		}
	}

	return false
}

func entityText(text string, entity t.MessageEntity) string {
	r := []rune(text)
	start := utf16Index(r, entity.Offset)
	end := utf16Index(r, entity.Offset+entity.Length)
	if start < 0 || end > len(r) || start > end {
		return ""
	}
	return string(r[start:end])
}

func utf16Index(runes []rune, utf16Pos int) int {
	count := 0
	for i, r := range runes {
		if r > 0xFFFF {
			count += 2
		} else {
			count++
		}
		if count > utf16Pos {
			return i
		}
	}
	return len(runes)
}

func (b *Bot) isReplyToMe(message t.Message) bool {
	if message.ReplyToMessage == nil {
		return false
	}
	if message.ReplyToMessage.From == nil {
		return false
	}

	replyToMessage := message.ReplyToMessage

	return replyToMessage != nil && replyToMessage.From.ID == b.me.ID
}

func (b *Bot) isPrivateWithMe(message t.Message) bool {
	return message.Chat.Type == t.ChatTypePrivate
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

func cropToMaxLengthMarkdownV2(text string, max int) (string, bool) {
	runes := []rune(text)
	if len(runes) <= max {
		return text, false
	}

	const ellipsis = "\\.\\.\\."
	ellipsisLen := len(ellipsis)

	cropPoint := max - ellipsisLen
	if cropPoint > len(runes) {
		cropPoint = len(runes)
	}
	for cropPoint > 0 && (runes[cropPoint] != ' ' || runes[cropPoint-1] == '\\') {
		cropPoint--
	}

	croppedRunes := runes[:cropPoint]

	// escape dangling formatting markers
	markers := []rune{'*', '_', '~', '|', '`'}
	for _, m := range markers {
		unescapedCount := 0
		lastIdx := -1
		for i := 0; i < len(croppedRunes); i++ {
			if croppedRunes[i] == '\\' {
				i++
				continue
			}
			if croppedRunes[i] == m {
				unescapedCount++
				lastIdx = i
			}
		}
		if unescapedCount%2 != 0 && lastIdx >= 0 {
			croppedRunes = append(croppedRunes[:lastIdx], append([]rune{'\\'}, croppedRunes[lastIdx:]...)...)
		}
	}

	if len(croppedRunes)+ellipsisLen > max {
		cropPoint := max - ellipsisLen
		if cropPoint > len(croppedRunes) {
			cropPoint = len(croppedRunes)
		}
		for cropPoint > 0 && croppedRunes[cropPoint-1] == '\\' {
			cropPoint--
		}
		croppedRunes = croppedRunes[:cropPoint]
	}

	return string(croppedRunes) + ellipsis, true
}

func (b *Bot) isFromAdmin(message *t.Message) bool {
	if message == nil || message.From == nil {
		return false
	}

	return slices.Contains(b.cfg.AdminIDs, message.From.ID)
}

func (b *Bot) describeImage(photo t.PhotoSize) (string, error) {
	file, err := b.api.GetFile(b.ctx, &t.GetFileParams{
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

	ctx, cancel := context.WithTimeout(b.ctx, b.cfg.LlmRequestTimeout)
	defer cancel()

	description, usage, err := b.llm.RecognizeImage(ctx, fileBytes)
	if err != nil {
		slog.Error("bot: Failed to recognize image", "error", err)
		return "", errors.Join(ErrImageRecognition, err)
	}

	if usage != nil {
		b.stats.AddUsage(usage.PromptTokens, usage.CompletionTokens, usage.TotalTokens, usage.Cost)
	}

	slog.Debug("bot: Image recognized", "description", description)

	return description, nil
}

// gets MessageData from Telego request context if previously stored by history middleware, otherwise creates it on the fly
func (b *Bot) getMessageDataFromRequestContextOrCreate(ctx *th.Context, message t.Message, isUserRequest bool) MessageData {
	if msgData, ok := ctx.Value(requestContextMessageDataKey).(MessageData); ok {
		msgData.IsUserRequest = isUserRequest
		slog.Debug("bot: Message data retrieved from context", "message_data", msgData)
		return msgData
	}

	msgData := b.tgUserMessageToMessageData(message, isUserRequest)
	slog.Debug("bot: Message data created from message on the fly", "message_data", msgData)
	return msgData
}
