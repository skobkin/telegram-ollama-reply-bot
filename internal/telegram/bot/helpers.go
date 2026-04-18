package bot

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
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
	ErrRequestTimeout   = errors.New("request timed out")
)

func (b *Bot) reply(originalMessage t.Message, newMessage *t.SendMessageParams) *t.SendMessageParams {
	return newMessage.WithReplyParameters(&t.ReplyParameters{
		MessageID: originalMessage.MessageID,
	})
}

// handlerContext returns a context tied to the current telego handler, falling back
// to the bot root context if the handler does not expose one.
func (b *Bot) handlerContext(handlerCtx *th.Context) context.Context {
	if handlerCtx != nil {
		if ctx := handlerCtx.Context(); ctx != nil {
			return ctx
		}
	}

	return b.ctx
}

func (b *Bot) sendTyping(ctx context.Context, chatId t.ChatID) {
	slog.Debug("bot: Setting 'typing' chat action")

	err := b.api.SendChatAction(ctx, tu.ChatAction(chatId, "typing"))
	if err != nil {
		slog.Error("bot: Cannot set chat action", "error", err)
		sentry.CaptureException(err)
	}
}

func (b *Bot) sendTypingUntil(ctx context.Context, chatId t.ChatID) {
	b.sendTyping(ctx, chatId)
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			b.sendTyping(ctx, chatId)
		}
	}
}

// runWithTimeout wraps handler work with typing feedback and the processing deadline.
func (b *Bot) runWithTimeout(baseCtx context.Context, chatId t.ChatID, work func(ctx context.Context) error) error {
	ctx, cancel := b.withProcessingDeadline(baseCtx)
	defer cancel()

	go b.sendTypingUntil(ctx, chatId)

	err := work(ctx)

	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			b.stats.LlmTimeout()

			return errors.Join(ErrRequestTimeout, err)
		}
		if ctxErr := ctx.Err(); errors.Is(ctxErr, context.DeadlineExceeded) {
			b.stats.LlmTimeout()

			return errors.Join(ErrRequestTimeout, err)
		}
		return err
	}

	if ctxErr := ctx.Err(); errors.Is(ctxErr, context.DeadlineExceeded) {
		b.stats.LlmTimeout()

		return ErrRequestTimeout
	}

	return nil
}

// withProcessingDeadline derives a child context that is cancelled either after the
// configured processing timeout or when the parent is cancelled.
func (b *Bot) withProcessingDeadline(baseCtx context.Context) (context.Context, context.CancelFunc) {
	if baseCtx == nil {
		baseCtx = b.ctx
	}

	timeout := b.cfg.ProcessingTimeout
	if timeout > 0 {
		return context.WithTimeout(baseCtx, timeout)
	}

	return context.WithCancel(baseCtx)
}

func (b *Bot) ensureHistoryMessagesImageDescriptions(ctx context.Context, chatID int64) {
	mh, ok := b.history[chatID]
	if !ok {
		return
	}

	for i := range mh.messages {
		b.ensureMessageImageDescription(ctx, &mh.messages[i])
	}
}

func (b *Bot) ensureMessageImageDescription(ctx context.Context, msg *MessageData) {
	if msg == nil {
		return
	}

	if msg.HasImage && msg.ImageMeta != nil && msg.Image == "" {
		if desc, ok := b.imageCache.Get(msg.ImageMeta); ok {
			msg.Image = desc
		} else {
			description, err := b.describeImage(ctx, msg.ImageMeta)
			if err != nil {
				slog.Error("bot: Failed to describe image", "error", err, "file_id", msg.ImageMeta.FileID)
				sentry.CaptureException(err)
			} else {
				b.imageCache.Set(msg.ImageMeta, description)
				msg.Image = description
				slog.Debug("bot: Image described", "file_id", msg.ImageMeta.FileID, "description", description)
			}
		}
	}

	if msg.ReplyTo != nil {
		b.ensureMessageImageDescription(ctx, msg.ReplyTo)
	}
}

func (b *Bot) trySendReplyError(ctx context.Context, message t.Message) {
	if ctx == nil {
		ctx = b.ctx
	}
	_, _ = b.api.SendMessage(ctx, b.reply(message, tu.Message(
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

func (b *Bot) describeImage(ctx context.Context, imageMeta *ImageMeta) (string, error) {
	if imageMeta == nil {
		return "", ErrImageRecognition
	}

	if ctx == nil {
		ctx = b.ctx
	}

	file, err := b.api.GetFile(ctx, &t.GetFileParams{FileID: imageMeta.FileID})
	if err != nil {
		return "", errors.Join(ErrImageRecognition, err)
	}

	fileBytes, err := downloadFileWithContext(ctx, b.api.FileDownloadURL(file.FilePath))
	if err != nil {
		return "", errors.Join(ErrImageRecognition, err)
	}

	description, usage, err := b.llm.RecognizeImage(ctx, fileBytes)
	if err != nil {
		return "", errors.Join(ErrImageRecognition, err)
	}

	if usage != nil {
		b.stats.AddUsage(usage.PromptTokens, usage.CompletionTokens, usage.TotalTokens, usage.Cost)
	}

	slog.Debug("bot: Image recognized", "file_id", imageMeta.FileID, "description", description)

	return description, nil
}

func downloadFileWithContext(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, resp.Body)

		return nil, fmt.Errorf("http request failed: %s", resp.Status)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	return data, nil
}

// gets MessageData from Telego request context if previously stored by history middleware, otherwise creates it on the fly
func (b *Bot) getMessageDataFromRequestContextOrCreate(ctx *th.Context, message t.Message, isUserRequest bool) MessageData {
	if msgData, ok := ctx.Value(requestContextMessageDataKey).(MessageData); ok {
		msgData.IsUserRequest = isUserRequest
		b.ensureMessageImageDescription(b.handlerContext(ctx), &msgData)
		slog.Debug("bot: Message data retrieved from context", "message_data", msgData)
		return msgData
	}

	msgData := b.tgUserMessageToMessageData(message, isUserRequest)
	b.ensureMessageImageDescription(b.handlerContext(ctx), &msgData)
	slog.Debug("bot: Message data created from message on the fly", "message_data", msgData)
	return msgData
}
