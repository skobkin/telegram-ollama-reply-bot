package bot

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"telegram-ollama-reply-bot/internal/llm"

	"github.com/getsentry/sentry-go"
	t "github.com/mymmrac/telego"
	th "github.com/mymmrac/telego/telegohandler"
	tu "github.com/mymmrac/telego/telegoutil"
)

func (b *Bot) summarizeHandler(ctx *th.Context, message t.Message) error {
	logger := b.handlerLogger(ctx)
	logger.Info("handling summarize command", "chat_id", message.Chat.ID)

	b.stats.SummarizeRequest()

	chatID := tu.ID(message.Chat.ID)
	args := strings.SplitN(message.Text, " ", 3)

	if len(args) < 2 {
		_, _ = ctx.Bot().SendMessage(ctx.Context(), tu.Message(
			tu.ID(message.Chat.ID),
			"Usage: /summarize <link>\r\n\r\n"+
				"Example:\r\n"+
				"/summarize https://kernel.org/get-notifications-for-your-patches.html",
		))

		return nil
	}

	url := strings.TrimSpace(args[1])
	additionalInstructions := ""
	if len(args) == 3 {
		additionalInstructions = strings.TrimSpace(args[2])
	}

	if !isValidAndAllowedURL(url) {
		logger.Warn("provided text is not a valid url")
		_, _ = ctx.Bot().SendMessage(ctx.Context(), b.reply(message, tu.Message(chatID, "URL is not valid.")))

		return nil
	}

	article, err := b.extractor.GetArticleFromURL(ctx.Context(), url)
	if err != nil {
		logger.Error("cannot retrieve article using extractor", "error", err, "url", url)
		sentry.CaptureException(err)
		_, _ = ctx.Bot().SendMessage(ctx.Context(), b.reply(message, tu.Message(chatID, "Failed to extract article content. Please check if the URL is correct.")))

		return nil
	}

	if article.Text == "" {
		logger.Warn("article text is empty", "url", url)
		sentry.CaptureMessage("Article text is empty")
		_, _ = ctx.Bot().SendMessage(ctx.Context(), b.reply(message, tu.Message(chatID, "No text extracted from the article. This resource is not supported at the moment.")))

		return nil
	}

	var summarizeReply string
	var summarizeUsage *llm.TokenUsage

	err = b.runWithTimeout(ctx.Context(), chatID, func(ctx context.Context) error {
		llmCtx, cancel := b.withProcessingDeadline(ctx)
		defer cancel()

		var llmErr error
		summarizeReply, summarizeUsage, llmErr = b.llm.Summarize(llmCtx, llm.PromptScope{ChatID: message.Chat.ID}, article.Text, additionalInstructions)

		return llmErr
	})
	if err != nil {
		if errors.Is(err, ErrRequestTimeout) {
			logger.Error("summarize request timed out", "chat_id", message.Chat.ID, "error", err)
			timeout := b.cfg.ProcessingTimeout
			_, _ = ctx.Bot().SendMessage(ctx.Context(), b.reply(message, tu.Message(chatID, fmt.Sprintf("LLM request timed out after %s. Try again later.", timeout))))

			return nil
		}

		logger.Error("cannot get summarize reply from llm connector", "error", err)
		sentry.CaptureException(err)
		_, _ = ctx.Bot().SendMessage(ctx.Context(), b.reply(message, tu.Message(chatID, "LLM request error. Try again later.")))

		return nil
	}

	if summarizeUsage != nil {
		b.stats.AddUsage(summarizeUsage.PromptTokens, summarizeUsage.CompletionTokens, summarizeUsage.TotalTokens, summarizeUsage.Cost)
	}

	logger.Debug("sending summarize reply", "reply_length", len(summarizeReply))

	footerURL := b.sanitizer.EscapeURL(article.URL)
	footer := "\n\n[src](" + footerURL + ")"
	body := b.sanitizer.Sanitize(summarizeReply)
	cropped, changed := cropToMaxLengthMarkdownV2(body, TelegramCharLimit-len(footer))
	if changed {
		cropped = b.sanitizer.Sanitize(cropped)
	}
	replyMarkdown := cropped + footer

	replyMessage := tu.Message(chatID, replyMarkdown).WithParseMode(t.ModeMarkdownV2)
	_, err = ctx.Bot().SendMessage(ctx.Context(), b.reply(message, replyMessage))
	if err != nil {
		logger.Error("cannot send summarize reply", "error", err, "reply_length", len(replyMarkdown))
		sentry.CaptureException(err)
		b.trySendReplyError(ctx.Context(), message)
	}

	b.saveBotReplyToHistory(ctx.Context(), message, replyMarkdown)

	return nil
}

func (b *Bot) helpHandler(ctx *th.Context, message t.Message) error {
	logger := b.handlerLogger(ctx)
	logger.Info("handling help command", "chat_id", message.Chat.ID)

	chatID := tu.ID(message.Chat.ID)
	b.sendTyping(ctx.Context(), chatID)

	_, err := ctx.Bot().SendMessage(ctx.Context(), b.reply(message, tu.Messagef(
		chatID,
		`Instructions:
Mention the bot, reply to it to chat; text and photos are supported.

- /summarize <link> [extra notes] - Summarize a page (alias: /s)
- /reset - Clear conversation history (admins only)
- /stats - Show usage stats (admins only)
- /admin_help - Show DM admin commands (admin DMs only)
- /help - Show this help`,
	)))
	if err != nil {
		logger.Error("cannot send help message", "error", err)
		sentry.CaptureException(err)
		b.trySendReplyError(ctx.Context(), message)
	}

	return nil
}

func (b *Bot) startHandler(ctx *th.Context, message t.Message) error {
	logger := b.handlerLogger(ctx)
	logger.Info("handling start command", "chat_id", message.Chat.ID)

	chatID := tu.ID(message.Chat.ID)
	b.sendTyping(ctx.Context(), chatID)

	_, err := ctx.Bot().SendMessage(ctx.Context(), b.reply(message, tu.Message(
		chatID,
		"Hey!\r\nCheck out /help to learn how to use this bot.",
	)))
	if err != nil {
		logger.Error("cannot send start message", "error", err)
		sentry.CaptureException(err)
		b.trySendReplyError(ctx.Context(), message)
	}

	return nil
}

func (b *Bot) statsHandler(ctx *th.Context, message t.Message) error {
	logger := b.handlerLogger(ctx)
	logger.Info("handling stats command", "chat_id", message.Chat.ID)

	if !b.isFromAdmin(&message) {
		logger.Warn("non-admin stats request denied", "chat_id", message.Chat.ID)
		_, _ = ctx.Bot().SendMessage(ctx.Context(), b.reply(message, tu.Message(tu.ID(message.Chat.ID), "This command is available only to administrators.")))

		return nil
	}

	chatID := tu.ID(message.Chat.ID)
	b.sendTyping(ctx.Context(), chatID)

	statsJSON := "```json\n" + b.stats.String() + "\n```"
	replyText := b.sanitizer.Sanitize("Current bot stats:\n" + statsJSON)
	_, err := ctx.Bot().SendMessage(ctx.Context(), b.reply(message, tu.Message(chatID, replyText)).WithParseMode(t.ModeMarkdownV2))
	if err != nil {
		logger.Error("cannot send stats message", "error", err)
		sentry.CaptureException(err)
		b.trySendReplyError(ctx.Context(), message)
	}

	return nil
}

func (b *Bot) resetHandler(ctx *th.Context, message t.Message) error {
	logger := b.handlerLogger(ctx)
	logger.Info("handling reset command", "chat_id", message.Chat.ID)

	if !b.isFromAdmin(&message) {
		logger.Warn("non-admin reset request denied", "chat_id", message.Chat.ID)
		_, _ = ctx.Bot().SendMessage(ctx.Context(), b.reply(message, tu.Message(tu.ID(message.Chat.ID), "This command is available only to administrators.")))

		return nil
	}

	chatID := message.Chat.ID
	b.sendTyping(ctx.Context(), tu.ID(chatID))

	b.ResetChatHistory(chatID)
	b.stats.ChatHistoryReset()

	_, err := ctx.Bot().SendMessage(ctx.Context(), b.reply(message, tu.Message(tu.ID(chatID), "Okay, let's start fresh.")))
	if err != nil {
		logger.Error("cannot send reset confirmation", "error", err)
		sentry.CaptureException(err)
		b.trySendReplyError(ctx.Context(), message)
	}

	return nil
}
