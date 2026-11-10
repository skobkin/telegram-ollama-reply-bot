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
// RichMessageCharLimit and the two Telegram-only constraints are normalized by
// markdown.SanitizeRichMarkdown. When the rich send fails, it falls back to
// the legacy MarkdownV2 pipeline; it returns an error only when that fails
// too.
func (b *Bot) sendRichMarkdownReply(ctx context.Context, message t.Message, rawMarkdown string) error {
	logger := b.loggerFromContext(ctx)

	text := rawMarkdown
	if cropped, changed := markdown.CropRichMarkdown(text, RichMessageCharLimit); changed {
		logger.Warn("rich reply cropped to fit length limit",
			"original_length", len([]rune(text)), "cropped_length", len([]rune(cropped)))
		text = cropped
	}
	text = markdown.SanitizeRichMarkdown(text)

	_, err := b.api.SendRichMessage(ctx, buildRichMessageParams(tu.ID(message.Chat.ID), message, text))
	if err == nil {
		return nil
	}

	logger.Warn("cannot send rich message, falling back to MarkdownV2",
		"error", err, "reply_length", len([]rune(text)))

	return b.sendLegacyMarkdownV2(ctx, message, rawMarkdown)
}

// sendLegacyMarkdownV2 sends text through the legacy MarkdownV2 sanitizer
// pipeline with the plain message character limit.
func (b *Bot) sendLegacyMarkdownV2(ctx context.Context, message t.Message, text string) error {
	sanitized := b.sanitizer.Sanitize(text)
	cropped, changed := markdown.CropToMaxLengthMarkdownV2(sanitized, TelegramCharLimit)
	if changed {
		cropped = b.sanitizer.Sanitize(cropped)
	}

	_, err := b.api.SendMessage(ctx, b.reply(message,
		tu.Message(tu.ID(message.Chat.ID), cropped).WithParseMode(t.ModeMarkdownV2)))

	return err
}
