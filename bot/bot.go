package bot

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"telegram-ollama-reply-bot/config"
	"telegram-ollama-reply-bot/extractor"
	"telegram-ollama-reply-bot/llm"
	"telegram-ollama-reply-bot/markdown"
	"telegram-ollama-reply-bot/stats"

	"github.com/getsentry/sentry-go"
	t "github.com/mymmrac/telego"
	th "github.com/mymmrac/telego/telegohandler"
	tu "github.com/mymmrac/telego/telegoutil"
)

var (
	ErrGetMe          = errors.New("cannot retrieve api user")
	ErrUpdatesChannel = errors.New("cannot get updates channel")
	ErrHandlerInit    = errors.New("cannot initialize handler")
	ErrHandlerStart   = errors.New("cannot start bot handler")
)

const TelegramCharLimit = 4000

type Bot struct {
	api        *t.Bot
	llm        *llm.LlmConnector
	extractor  extractor.Extractor
	sanitizer  markdown.Sanitizer
	stats      *stats.Stats
	history    map[int64]*MessageHistory
	me         botInfo
	cfg        config.BotConfig
	ctx        context.Context
	imageCache *ImageCache
}

func NewBot(
	api *t.Bot,
	llm *llm.LlmConnector,
	extractor extractor.Extractor,
	sanitizer markdown.Sanitizer,
	imageCache *ImageCache,
	cfg config.BotConfig,
	ctx context.Context,
) *Bot {
	if imageCache == nil {
		panic("image cache is required")
	}

	return &Bot{
		api:        api,
		llm:        llm,
		extractor:  extractor,
		sanitizer:  sanitizer,
		stats:      stats.NewStats(),
		history:    make(map[int64]*MessageHistory),
		me:         botInfo{},
		cfg:        cfg,
		ctx:        ctx,
		imageCache: imageCache,
	}
}

func (b *Bot) Run() error {
	botUser, err := b.api.GetMe(b.ctx)
	if err != nil {
		slog.Error("bot: Cannot retrieve api user", "error", err)
		sentry.CaptureException(err)

		return ErrGetMe
	}

	b.me = botInfoFromUser(botUser)

	slog.Info("bot: Running api as",
		"id", b.me.ID,
		"username", b.me.Username,
		"first_name", b.me.FirstName,
		"last_name", b.me.LastName,
		"is_bot", b.me.IsBot,
		"can_join_groups", b.me.CanJoinGroups,
		"can_read_all_group_messages", b.me.CanReadAllGroupMessages,
		"supports_inline_queries", b.me.SupportsInlineQueries)
	sentry.AddBreadcrumb(&sentry.Breadcrumb{
		Category: "telegram-api",
		Message:  "Bot ID: " + strconv.FormatInt(botUser.ID, 10),
		Level:    sentry.LevelInfo,
	})

	updates, err := b.api.UpdatesViaLongPolling(b.ctx, nil)
	if err != nil {
		slog.Error("bot: Cannot get update channel", "error", err)
		sentry.CaptureException(err)

		return ErrUpdatesChannel
	}

	bh, err := th.NewBotHandler(b.api, updates)
	if err != nil {
		slog.Error("bot: Cannot initialize bot handler", "error", err)
		sentry.CaptureException(err)

		return ErrHandlerInit
	}

	defer func() {
		slog.Info("bot: Stopping bot handler")
		err := bh.Stop()
		if err != nil {
			slog.Error("bot: Cannot stop bot handler", "error", err)
			sentry.CaptureException(err)
		}
	}()

	// Middlewares
	bh.Use(b.chatHistory)
	bh.Use(b.chatTypeStatsCounter)

	// Command handlers
	slog.Debug("bot: Registering message handlers")
	commandForMe := b.commandForThisBot()
	bh.HandleMessage(b.startHandler, th.And(commandForMe, th.CommandEqual("start")))
	bh.HandleMessage(b.summarizeHandler, th.And(commandForMe, th.Or(th.CommandEqual("summarize"), th.CommandEqual("s"))))
	bh.HandleMessage(b.statsHandler, th.And(commandForMe, th.CommandEqual("stats")))
	bh.HandleMessage(b.helpHandler, th.And(commandForMe, th.CommandEqual("help")))
	bh.HandleMessage(b.resetHandler, th.And(commandForMe, th.CommandEqual("reset")))
	// Since we're need to process both text and photo messages, we need to use Update handler instead of Message handler
	bh.Handle(b.textMessageHandler, th.Or(th.AnyMessageWithText(), AnyMessageWithPhoto()))
	slog.Debug("bot: Message handlers registered")

	slog.Info("bot: Starting bot handler")
	if err := bh.Start(); err != nil {
		slog.Error("bot: Cannot start bot handler", "error", err)
		sentry.CaptureException(err)

		return ErrHandlerStart
	}

	return nil
}

func (b *Bot) textMessageHandler(ctx *th.Context, update t.Update) error {
	if update.Message == nil {
		return nil
	}
	message := *update.Message

	if b.isMentionOfMe(message) || b.isReplyToMe(message) || b.isPrivateWithMe(message) {
		messageType := "private"
		if b.isMentionOfMe(message) {
			messageType = "mention"
		} else if b.isReplyToMe(message) {
			messageType = "reply"
		}
		slog.Info("bot: Processing message", "type", messageType)
		b.processMention(ctx, message)
	} else {
		slog.Debug("bot: Skipping message - not a mention, reply, or private chat")
	}
	return nil
}

func (b *Bot) processMention(reqCtx *th.Context, message t.Message) {
	b.stats.Mention()

	slog.Info("bot: /mention", "chat", message.Chat.ID)

	chatID := tu.ID(message.Chat.ID)

	b.maybeSummarizeHistory(message.Chat.ID)

	baseCtx := b.handlerContext(reqCtx)

	// Get MessageData from the request context if available, otherwise create it on the fly
	userMessageData := b.getMessageDataFromRequestContextOrCreate(reqCtx, message, true)

	var llmReply string
	var usage *llm.TokenUsage
	var err error

	err = b.runWithTimeout(baseCtx, chatID, func(ctx context.Context) error {
		requestContext := b.createLlmRequestContextFromMessage(ctx, message)

		llmCtx, cancel := b.withProcessingDeadline(ctx)
		defer cancel()

		var llmErr error
		llmReply, usage, llmErr = b.llm.HandleChatMessage(
			llmCtx,
			messageDataToLlmMessage(userMessageData),
			requestContext,
		)
		return llmErr
	})
	if err != nil {
		if errors.Is(err, ErrRequestTimeout) {
			slog.Error("bot: LLM request timed out", "chat", message.Chat.ID, "error", err)
			timeout := b.cfg.ProcessingTimeout
			_, _ = b.api.SendMessage(baseCtx, b.reply(message, tu.Message(
				chatID,
				fmt.Sprintf("LLM request timed out after %s. Try again later.", timeout),
			)))

			return
		}

		slog.Error("bot: Cannot get reply from LLM connector", "error", err)
		sentry.CaptureException(err)

		_, _ = b.api.SendMessage(baseCtx, b.reply(message, tu.Message(
			chatID,
			"LLM request error. Try again later.",
		)))

		return
	}

	if usage != nil {
		b.stats.AddUsage(usage.PromptTokens, usage.CompletionTokens, usage.TotalTokens, usage.Cost)
	}

	slog.Debug("bot: Got completion. Going to send.", "llm-completion", llmReply)

	sanitizedReply := b.sanitizer.Sanitize(llmReply)

	reply := tu.Message(
		chatID,
		sanitizedReply,
	).WithParseMode(t.ModeMarkdownV2)

	_, err = b.api.SendMessage(baseCtx, b.reply(message, reply))
	if err != nil {
		slog.Error("bot: Can't send reply message", "error", err, "sanitized_reply", sanitizedReply)
		sentry.CaptureException(err)

		b.trySendReplyError(baseCtx, message)

		return
	}

	b.saveBotReplyToHistory(message, llmReply)
}

func (b *Bot) summarizeHandler(ctx *th.Context, message t.Message) error {
	slog.Info("bot: /summarize", "message-text", message.Text)

	b.stats.SummarizeRequest()

	chatID := tu.ID(message.Chat.ID)

	args := strings.SplitN(message.Text, " ", 3)
	argsCount := len(args)

	if argsCount < 2 {
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
	if argsCount == 3 {
		additionalInstructions = strings.TrimSpace(args[2])
	}

	if !isValidAndAllowedUrl(url) {
		slog.Error("bot: Provided text is not a valid URL", "text", url)

		_, _ = ctx.Bot().SendMessage(ctx.Context(), b.reply(message, tu.Message(
			chatID,
			"URL is not valid.",
		)))

		return nil
	}

	var err error
	article, err := b.extractor.GetArticleFromUrl(url)
	if err != nil {
		slog.Error("bot: Cannot retrieve an article using extractor", "error", err)
		sentry.CaptureException(err)

		_, _ = ctx.Bot().SendMessage(ctx.Context(), b.reply(message, tu.Message(
			chatID,
			"Failed to extract article content. Please check if the URL is correct.",
		)))

		return nil
	}

	if article.Text == "" {
		slog.Error("bot: Article text is empty", "url", url)
		sentry.CaptureMessage("Article text is empty")

		_, _ = ctx.Bot().SendMessage(ctx.Context(), b.reply(message, tu.Message(
			chatID,
			"No text extracted from the article. This resource is not supported at the moment.",
		)))

		return nil
	}

	var summarizeReply string
	var summarizeUsage *llm.TokenUsage

	err = b.runWithTimeout(ctx.Context(), chatID, func(ctx context.Context) error {
		llmCtx, cancel := b.withProcessingDeadline(ctx)
		defer cancel()

		var llmErr error
		summarizeReply, summarizeUsage, llmErr = b.llm.Summarize(llmCtx, article.Text, additionalInstructions)
		return llmErr
	})
	if err != nil {
		if errors.Is(err, ErrRequestTimeout) {
			slog.Error("bot: Summarize request timed out", "chat", message.Chat.ID, "error", err)
			timeout := b.cfg.ProcessingTimeout
			_, _ = ctx.Bot().SendMessage(ctx.Context(), b.reply(message, tu.Message(
				chatID,
				fmt.Sprintf("LLM request timed out after %s. Try again later.", timeout),
			)))

			return nil
		}

		slog.Error("bot: Cannot get reply from LLM connector", "error", err)
		sentry.CaptureException(err)

		_, _ = ctx.Bot().SendMessage(ctx.Context(), b.reply(message, tu.Message(
			chatID,
			"LLM request error. Try again later.",
		)))

		return nil
	}

	if summarizeUsage != nil {
		b.stats.AddUsage(summarizeUsage.PromptTokens, summarizeUsage.CompletionTokens, summarizeUsage.TotalTokens, summarizeUsage.Cost)
	}

	slog.Debug("bot: Got completion. Going to send reply.", "llm-completion", summarizeReply)

	footerURL := b.sanitizer.EscapeURL(article.Url)
	footer := "\n\n[src](" + footerURL + ")"
	body := b.sanitizer.Sanitize(summarizeReply)
	cropped, changed := cropToMaxLengthMarkdownV2(body, TelegramCharLimit-len(footer))
	if changed {
		cropped = b.sanitizer.Sanitize(cropped)
	}
	replyMarkdown := cropped + footer

	replyMessage := tu.Message(
		chatID,
		replyMarkdown,
	).WithParseMode(t.ModeMarkdownV2)

	_, err = ctx.Bot().SendMessage(ctx.Context(), b.reply(message, replyMessage))

	if err != nil {
		slog.Error("bot: Can't send reply message", "error", err, "sanitized_reply", replyMarkdown)
		sentry.CaptureException(err)

		b.trySendReplyError(ctx.Context(), message)
	}

	b.saveBotReplyToHistory(message, replyMarkdown)
	return nil
}

func (b *Bot) helpHandler(ctx *th.Context, message t.Message) error {
	slog.Info("bot: /help")

	chatID := tu.ID(message.Chat.ID)

	b.sendTyping(ctx.Context(), chatID)

	_, err := ctx.Bot().SendMessage(ctx.Context(), b.reply(message, tu.Messagef(
		chatID,
		`Instructions:
Mention the bot, reply to it to chat; text and photos are supported.

- /summarize <link> [extra notes] - Summarize a page (alias: /s)
- /reset - Clear conversation history (admins only)
- /stats - Show usage stats (admins only)
- /help - Show this help`,
	)))
	if err != nil {
		slog.Error("bot: Cannot send a message", "error", err)
		sentry.CaptureException(err)

		b.trySendReplyError(ctx.Context(), message)
	}
	return nil
}

func (b *Bot) startHandler(ctx *th.Context, message t.Message) error {
	slog.Info("bot: /start")

	chatID := tu.ID(message.Chat.ID)

	b.sendTyping(ctx.Context(), chatID)

	_, err := ctx.Bot().SendMessage(ctx.Context(), b.reply(message, tu.Message(
		chatID,
		"Hey!\r\n"+
			"Check out /help to learn how to use this bot.",
	)))
	if err != nil {
		slog.Error("bot: Cannot send a message", "error", err)
		sentry.CaptureException(err)

		b.trySendReplyError(ctx.Context(), message)
	}
	return nil
}

func (b *Bot) statsHandler(ctx *th.Context, message t.Message) error {
	slog.Info("bot: /stats")

	if !b.isFromAdmin(&message) {
		slog.Info("bot: /stats request from non-admin user, denying")
		_, _ = ctx.Bot().SendMessage(ctx.Context(), b.reply(message, tu.Message(
			tu.ID(message.Chat.ID),
			"This command is available only to administrators.",
		)))
		return nil
	}

	chatID := tu.ID(message.Chat.ID)

	b.sendTyping(ctx.Context(), chatID)

	statsJSON := "```json\n" + b.stats.String() + "\n```"
	replyText := b.sanitizer.Sanitize("Current bot stats:\n" + statsJSON)
	_, err := ctx.Bot().SendMessage(ctx.Context(), b.reply(message, tu.Message(
		chatID,
		replyText,
	)).WithParseMode(t.ModeMarkdownV2))
	if err != nil {
		slog.Error("bot: Cannot send a message", "error", err)
		sentry.CaptureException(err)

		b.trySendReplyError(ctx.Context(), message)
	}
	return nil
}

func (b *Bot) resetHandler(ctx *th.Context, message t.Message) error {
	slog.Info("bot: /reset")

	if !b.isFromAdmin(&message) {
		slog.Info("bot: /reset request from non-admin user, denying")
		_, _ = ctx.Bot().SendMessage(ctx.Context(), b.reply(message, tu.Message(
			tu.ID(message.Chat.ID),
			"This command is available only to administrators.",
		)))
		return nil
	}

	chatID := message.Chat.ID

	b.sendTyping(ctx.Context(), tu.ID(chatID))

	b.ResetChatHistory(chatID)
	b.stats.ChatHistoryReset()

	_, err := ctx.Bot().SendMessage(ctx.Context(), b.reply(message, tu.Message(
		tu.ID(chatID),
		"Okay, let's start fresh.",
	)))
	if err != nil {
		slog.Error("bot: Cannot send a message", "error", err)
		sentry.CaptureException(err)

		b.trySendReplyError(ctx.Context(), message)
	}
	return nil
}
