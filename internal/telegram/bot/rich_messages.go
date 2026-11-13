package bot

import (
	"context"

	"telegram-ollama-reply-bot/internal/support/markdown"

	t "github.com/mymmrac/telego"
	tu "github.com/mymmrac/telego/telegoutil"
)

// RichMessageCharLimit is the maximum length of a Telegram rich message in
// characters.
const RichMessageCharLimit = 32768

// buildRichMessageParams assembles sendRichMessage parameters for a markdown
// rich message replying to the given message.
func buildRichMessageParams(chatID t.ChatID, message t.Message, markdownText string) *t.SendRichMessageParams {
	params := &t.SendRichMessageParams{
		ChatID:      chatID,
		RichMessage: tu.RichMessageMarkdown(markdownText),
		ReplyParameters: &t.ReplyParameters{
			MessageID: message.MessageID,
		},
	}
	if message.MessageThreadID != 0 {
		params.MessageThreadID = message.MessageThreadID
	}

	return params
}

// sendRichMarkdownReply sends raw markdown from the model as a Telegram rich
// message (GitHub Flavored Markdown rendering). Over-length text is cropped to
// RichMessageCharLimit. When the rich send fails, it falls back to the legacy
// MarkdownV2 pipeline; it returns an error only when that fails too. The
// returned message is Telegram's copy of the sent message (nil on error).
func (b *Bot) sendRichMarkdownReply(ctx context.Context, message t.Message, rawMarkdown string) (*t.Message, error) {
	return b.sendRichMarkdownReplyWithSuffix(ctx, message, rawMarkdown, "")
}

// sendRichMarkdownReplyWithSuffix sends raw markdown followed by a reserved
// suffix (e.g. the /summarize source link) as a rich message. The markdown is
// passed through as-is: Rich Markdown is GFM-compatible, tolerates unbalanced
// markup, and documents inline syntax such as tg://emoji and tg://time image
// references, so nothing is rewritten pre-send; the MarkdownV2 fallback below
// is the safety net when Telegram rejects a message. Both the rich crop and
// the legacy fallback crop reserve room for the suffix so it always reaches
// the wire. The returned message comes from the pipeline that actually
// delivered the text (nil on error).
func (b *Bot) sendRichMarkdownReplyWithSuffix(ctx context.Context, message t.Message, rawMarkdown, suffix string) (*t.Message, error) {
	logger := b.loggerFromContext(ctx)

	body := rawMarkdown
	if cropped, changed := markdown.CropRichMarkdown(body, RichMessageCharLimit-len([]rune(suffix))); changed {
		logger.Warn("rich reply cropped to fit length limit",
			"original_length", len([]rune(body)), "cropped_length", len([]rune(cropped)))
		body = cropped
	}

	sent, err := b.api.SendRichMessage(ctx, buildRichMessageParams(tu.ID(message.Chat.ID), message, body+suffix))
	if err == nil {
		return sent, nil
	}
	if isRateLimit(err) {
		// A 429 is chat-wide and transient: the legacy fallback walks into
		// the same rate-limited window, so it is not attempted. The rate
		// limit is returned unchanged so the caller's gate logic can absorb
		// it — flattening it into a fallback error would hide the signal.
		return nil, err
	}
	if ctx.Err() != nil {
		// A send context that is already done cannot carry the fallback:
		// the rich attempt was aborted (deadline or cancellation), the
		// fallback is a guaranteed-failing call on the same dead context,
		// and its differing error would mask the abort. The original error
		// is returned unchanged.
		return nil, err
	}

	logger.Warn("cannot send rich message, falling back to MarkdownV2",
		"error", err, "reply_length", len([]rune(body))+len([]rune(suffix)))

	return b.sendLegacyMarkdownV2(ctx, message, rawMarkdown, suffix)
}

// sendLegacyMarkdownV2 sends text (plus an optional reserved suffix appended
// verbatim after cropping) through the legacy MarkdownV2 sanitizer pipeline
// with the plain message character limit. The originating forum topic is
// preserved exactly as the rich path does: a fallback send must not move
// the reply out of its thread.
func (b *Bot) sendLegacyMarkdownV2(ctx context.Context, message t.Message, text, suffix string) (*t.Message, error) {
	sanitized := b.sanitizer.Sanitize(text)

	limit := TelegramCharLimit
	if suffix != "" {
		limit -= len(suffix)
	}

	cropped, changed := markdown.CropToMaxLengthMarkdownV2(sanitized, limit)
	if changed {
		cropped = b.sanitizer.Sanitize(cropped)
	}

	params := b.reply(message,
		tu.Message(tu.ID(message.Chat.ID), cropped+suffix).WithParseMode(t.ModeMarkdownV2))
	if message.MessageThreadID != 0 {
		params.MessageThreadID = message.MessageThreadID
	}

	return b.api.SendMessage(ctx, params)
}
