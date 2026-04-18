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
	"telegram-ollama-reply-bot/internal/logging"
	"time"

	"github.com/getsentry/sentry-go"
	t "github.com/mymmrac/telego"
	th "github.com/mymmrac/telego/telegohandler"
	tu "github.com/mymmrac/telego/telegoutil"
)

var (
	allowedURLSchemes = []string{"http", "https"}

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

func (b *Bot) loggerFromContext(ctx context.Context) *slog.Logger {
	return logging.FromContext(ctx, b.logger)
}

func (b *Bot) handlerLogger(ctx *th.Context) *slog.Logger {
	return b.loggerFromContext(b.handlerContext(ctx))
}

func (b *Bot) sendTyping(ctx context.Context, chatID t.ChatID) {
	logger := b.loggerFromContext(ctx)
	logger.Debug("setting typing chat action", "chat_id", chatID)

	err := b.api.SendChatAction(ctx, tu.ChatAction(chatID, "typing"))
	if err != nil {
		logger.Error("cannot set chat action", "error", err, "chat_id", chatID)
		sentry.CaptureException(err)
	}
}

func (b *Bot) sendTypingUntil(ctx context.Context, chatID t.ChatID) {
	b.sendTyping(ctx, chatID)
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			b.sendTyping(ctx, chatID)
		}
	}
}

// runWithTimeout wraps handler work with typing feedback and the processing deadline.
func (b *Bot) runWithTimeout(baseCtx context.Context, chatID t.ChatID, work func(ctx context.Context) error) error {
	ctx, cancel := b.withProcessingDeadline(baseCtx)
	defer cancel()

	go b.sendTypingUntil(ctx, chatID)

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
				b.loggerFromContext(ctx).Error("failed to describe image", "error", err, "file_id", msg.ImageMeta.FileID)
				sentry.CaptureException(err)
			} else {
				b.imageCache.Set(msg.ImageMeta, description)
				msg.Image = description
				b.loggerFromContext(ctx).Debug("image described", "file_id", msg.ImageMeta.FileID, "description_length", len(description))
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

func isValidAndAllowedURL(text string) bool {
	u, err := url.ParseRequestURI(text)
	if err != nil {
		sentry.CaptureException(err)

		return false
	}

	if !slices.Contains(allowedURLSchemes, strings.ToLower(u.Scheme)) {
		return false
	}

	return true
}

func cropToMaxLengthMarkdownV2(text string, limit int) (string, bool) {
	runes := []rune(text)
	if len(runes) <= limit {
		return text, false
	}

	const ellipsis = "\\.\\.\\."
	ellipsisLen := len(ellipsis)

	cropPoint := limit - ellipsisLen
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

	if len(croppedRunes)+ellipsisLen > limit {
		cropPoint := limit - ellipsisLen
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

	b.loggerFromContext(ctx).Debug("image recognized", "file_id", imageMeta.FileID, "image_bytes", len(fileBytes), "description_length", len(description))

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
	defer func() {
		_ = resp.Body.Close()
	}()

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
		b.handlerLogger(ctx).Debug("message data retrieved from request context", "has_image", msgData.HasImage)

		return msgData
	}

	msgData := b.tgUserMessageToMessageData(message, isUserRequest)
	b.ensureMessageImageDescription(b.handlerContext(ctx), &msgData)
	b.handlerLogger(ctx).Debug("message data created on the fly", "has_image", msgData.HasImage)

	return msgData
}
