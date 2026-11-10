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
// MarkdownV2 pipeline; it returns an error only when that fails too.
func (b *Bot) sendRichMarkdownReply(ctx context.Context, message t.Message, rawMarkdown string) error {
	return b.sendRichMarkdownReplyWithSuffix(ctx, message, rawMarkdown, "")
}

// sendRichMarkdownReplyWithSuffix sends raw markdown followed by a reserved
// suffix (e.g. the /summarize source link) as a rich message. The markdown is
// passed through as-is: Rich Markdown is GFM-compatible, tolerates unbalanced
// markup, and documents inline syntax such as tg://emoji and tg://time image
// references, so nothing is rewritten pre-send; the MarkdownV2 fallback below
// is the safety net when Telegram rejects a message. Both the rich crop and
// the legacy fallback crop reserve room for the suffix so it always reaches
// the wire.
func (b *Bot) sendRichMarkdownReplyWithSuffix(ctx context.Context, message t.Message, rawMarkdown, suffix string) error {
	logger := b.loggerFromContext(ctx)

	body := rawMarkdown
	if cropped, changed := markdown.CropRichMarkdown(body, RichMessageCharLimit-len([]rune(suffix))); changed {
		logger.Warn("rich reply cropped to fit length limit",
			"original_length", len([]rune(body)), "cropped_length", len([]rune(cropped)))
		body = cropped
	}

	_, err := b.api.SendRichMessage(ctx, buildRichMessageParams(tu.ID(message.Chat.ID), message, body+suffix))
	if err == nil {
		return nil
	}

	logger.Warn("cannot send rich message, falling back to MarkdownV2",
		"error", err, "reply_length", len([]rune(body))+len([]rune(suffix)))

	return b.sendLegacyMarkdownV2(ctx, message, rawMarkdown, suffix)
}

// sendLegacyMarkdownV2 sends text (plus an optional reserved suffix appended
// verbatim after cropping) through the legacy MarkdownV2 sanitizer pipeline
// with the plain message character limit.
func (b *Bot) sendLegacyMarkdownV2(ctx context.Context, message t.Message, text, suffix string) error {
	sanitized := b.sanitizer.Sanitize(text)

	limit := TelegramCharLimit
	if suffix != "" {
		limit -= len(suffix)
	}

	cropped, changed := markdown.CropToMaxLengthMarkdownV2(sanitized, limit)
	if changed {
		cropped = b.sanitizer.Sanitize(cropped)
	}

	_, err := b.api.SendMessage(ctx, b.reply(message,
		tu.Message(tu.ID(message.Chat.ID), cropped+suffix).WithParseMode(t.ModeMarkdownV2)))

	return err
}
