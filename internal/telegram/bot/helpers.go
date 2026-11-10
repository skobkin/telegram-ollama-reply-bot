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
	"telegram-ollama-reply-bot/internal/adminconfig"
	"telegram-ollama-reply-bot/internal/logging"
	"telegram-ollama-reply-bot/internal/state"
	"telegram-ollama-reply-bot/internal/support/httpclient"
	"telegram-ollama-reply-bot/internal/support/safeerr"
	"telegram-ollama-reply-bot/internal/vision"
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

func (b *Bot) hydrateMessagesWithImageDescriptions(ctx context.Context, messages []state.Message) []state.Message {
	result := make([]state.Message, 0, len(messages))
	for i := range messages {
		msg := messages[i]
		b.ensureMessageImageDescription(ctx, &msg)
		result = append(result, msg)
	}

	return result
}

func (b *Bot) ensureMessageImageDescription(ctx context.Context, msg *state.Message) {
	if msg == nil {
		return
	}

	if msg.HasImage && msg.ImageMeta != nil && msg.Image == "" {
		if b.imageRecognition.Enabled {
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
		} else {
			msg.Image = "🖼️ Image attached (recognition disabled)"
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

func (b *Bot) isFromAdmin(message *t.Message) bool {
	if message == nil || message.From == nil {
		return false
	}

	return slices.Contains(b.cfg.AdminIDs, message.From.ID)
}

func (b *Bot) isAdminDM(message *t.Message) bool {
	return message != nil && message.Chat.Type == t.ChatTypePrivate && b.isFromAdmin(message)
}

func chatDisplayName(chat t.Chat) string {
	switch {
	case chat.Title != "":
		return chat.Title
	case chat.Username != "":
		return chat.Username
	case chat.FirstName != "" || chat.LastName != "":
		return strings.TrimSpace(chat.FirstName + " " + chat.LastName)
	default:
		return ""
	}
}

func emptyFallback(value string) string {
	if value == "" {
		return "<inherit>"
	}

	return value
}

// describeImage downloads a Telegram image and returns its description from
// the vision service. Downloads are bounded by the configured max image size
// (checked against the reported file size before any bytes are transferred)
// and transport errors are sanitized — the Telegram file URL embeds the bot
// token — before they can reach logs or Sentry.
func (b *Bot) describeImage(ctx context.Context, imageMeta *state.ImageMeta) (string, error) {
	if imageMeta == nil {
		return "", ErrImageRecognition
	}

	if ctx == nil {
		ctx = b.ctx
	}

	maxBytes := b.imageRecognition.MaxImageBytes
	if maxBytes > 0 && int64(imageMeta.FileSize) > maxBytes {
		b.loggerFromContext(ctx).Warn(
			"image file exceeds size bound; skipping download",
			"file_id", imageMeta.FileID,
			"file_size", imageMeta.FileSize,
			"max_image_bytes", maxBytes,
		)

		return "", errors.Join(ErrImageRecognition, fmt.Errorf("%w: file reports %d bytes", vision.ErrImageTooLarge, imageMeta.FileSize))
	}

	file, err := b.api.GetFile(ctx, &t.GetFileParams{FileID: imageMeta.FileID})
	if err != nil {
		return "", errors.Join(ErrImageRecognition, err)
	}

	fileBytes, err := downloadFileWithContext(ctx, b.api.FileDownloadURL(file.FilePath), maxBytes)
	if err != nil {
		return "", errors.Join(ErrImageRecognition, err)
	}

	recognition, err := b.vision.Describe(ctx, fileBytes)
	if err != nil {
		return "", errors.Join(ErrImageRecognition, err)
	}

	if recognition.Usage != nil {
		b.stats.AddUsage(recognition.Usage.PromptTokens, recognition.Usage.CompletionTokens, recognition.Usage.TotalTokens, recognition.Usage.Cost)
	}

	b.loggerFromContext(ctx).Debug("image recognized", "file_id", imageMeta.FileID, "image_bytes", len(fileBytes), "description_length", len(recognition.Text))

	return recognition.Text, nil
}

func (b *Bot) shouldProcessChatMessage(ctx context.Context, message t.Message) bool {
	if b.admin == nil {
		return true
	}

	resolved, err := b.admin.ResolveChatConfig(ctx, message.Chat.ID)
	if err != nil {
		b.loggerFromContext(ctx).Warn("failed to resolve chat config", "chat_id", message.Chat.ID, "error", err)

		return false
	}

	switch resolved.InteractivityMode {
	case adminconfig.InteractivityMentionsOnly:
		return b.isMentionOfMe(message) || b.hasSoftTrigger(message, resolved)
	case adminconfig.InteractivityMentionsReplies:
		return b.isMentionOfMe(message) || b.isReplyToMe(message) || b.isPrivateWithMe(message) || b.hasSoftTrigger(message, resolved)
	case adminconfig.InteractivityDisabled:
		return b.isAdminDM(&message)
	default:
		return false
	}
}

func (b *Bot) hasSoftTrigger(message t.Message, resolved adminconfig.ResolvedChatConfig) bool {
	text := strings.ToLower(message.Text)
	if text == "" {
		text = strings.ToLower(message.Caption)
	}
	if text == "" {
		return false
	}

	candidate := strings.TrimSpace(strings.ToLower(resolved.CharacterName))
	if candidate != "" && strings.Contains(text, candidate) {
		return true
	}

	return false
}

// errFileDownload names Telegram file-download failures. Raw transport errors
// are never surfaced: the download URL embeds the bot token, so causes are
// discarded via safeerr and only the typed kind and HTTP status remain.
var errFileDownload = errors.New("telegram file download failed")

// fileDownloadClient is the shared client for Telegram file downloads:
// environment-proxy aware, without http.DefaultClient's global sharing.
var fileDownloadClient = httpclient.New()

// downloadFileWithContext downloads a Telegram Bot API file URL bounded by
// maxBytes. A non-positive maxBytes disables the bound (preserving the
// pre-bound behavior for direct struct construction); production wiring always
// passes the configured bound.
func downloadFileWithContext(ctx context.Context, fileURL string, maxBytes int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fileURL, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: cannot build request", errFileDownload)
	}

	resp, err := fileDownloadClient.Do(req)
	if err != nil {
		return nil, safeerr.WrapTransport(errFileDownload, err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, resp.Body)

		return nil, fmt.Errorf("%w: http status %d", errFileDownload, resp.StatusCode)
	}

	var reader io.Reader = resp.Body
	if maxBytes > 0 {
		reader = io.LimitReader(resp.Body, maxBytes+1)
	}

	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, safeerr.WrapTransport(errFileDownload, err)
	}

	if maxBytes > 0 && int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("%w: file exceeds bound of %d bytes", errFileDownload, maxBytes)
	}

	return data, nil
}

// gets a state message from Telego request context if previously stored by history middleware,
// otherwise creates it on the fly
func (b *Bot) getMessageDataFromRequestContextOrCreate(ctx *th.Context, message t.Message, isUserRequest bool) state.Message {
	if msgData, ok := ctx.Value(requestContextMessageDataKey).(state.Message); ok {
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
