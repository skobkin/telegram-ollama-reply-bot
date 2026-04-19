package bot

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"telegram-ollama-reply-bot/internal/config"
	"telegram-ollama-reply-bot/internal/content/extractor"
	"telegram-ollama-reply-bot/internal/llm"
	"telegram-ollama-reply-bot/internal/support/markdown"
	"telegram-ollama-reply-bot/internal/support/stats"

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
	llm        *llm.Service
	extractor  extractor.Extractor
	sanitizer  markdown.Sanitizer
	stats      *stats.Stats
	history    map[int64]*MessageHistory
	me         botInfo
	cfg        config.BotConfig
	ctx        context.Context
	imageCache *ImageCache
	logger     *slog.Logger
}

func NewBot(
	ctx context.Context,
	api *t.Bot,
	llm *llm.Service,
	extractor extractor.Extractor,
	sanitizer markdown.Sanitizer,
	imageCache *ImageCache,
	cfg config.BotConfig,
	logger *slog.Logger,
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
		logger:     logger,
	}
}

func (b *Bot) Run() error {
	logger := b.loggerFromContext(b.ctx)

	botUser, err := b.api.GetMe(b.ctx)
	if err != nil {
		logger.Error("cannot retrieve bot api user", "error", err)
		sentry.CaptureException(err)

		return ErrGetMe
	}

	b.me = botInfoFromUser(botUser)

	logger.Info("telegram api initialized",
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
		logger.Error("cannot get updates channel", "error", err)
		sentry.CaptureException(err)

		return ErrUpdatesChannel
	}

	bh, err := th.NewBotHandler(b.api, updates)
	if err != nil {
		logger.Error("cannot initialize bot handler", "error", err)
		sentry.CaptureException(err)

		return ErrHandlerInit
	}

	defer func() {
		logger.Info("stopping bot handler")
		err := bh.Stop()
		if err != nil {
			logger.Error("cannot stop bot handler", "error", err)
			sentry.CaptureException(err)
		}
	}()

	// Middlewares
	bh.Use(b.requestLogger)
	bh.Use(b.chatHistory)
	bh.Use(b.chatTypeStatsCounter)

	// Command handlers
	logger.Debug("registering message handlers")
	commandForMe := b.commandForThisBot()
	bh.HandleMessage(b.startHandler, th.And(commandForMe, th.CommandEqual("start")))
	bh.HandleMessage(b.summarizeHandler, th.And(commandForMe, th.Or(th.CommandEqual("summarize"), th.CommandEqual("s"))))
	bh.HandleMessage(b.statsHandler, th.And(commandForMe, th.CommandEqual("stats")))
	bh.HandleMessage(b.helpHandler, th.And(commandForMe, th.CommandEqual("help")))
	bh.HandleMessage(b.resetHandler, th.And(commandForMe, th.CommandEqual("reset")))
	// Since we're need to process both text and photo messages, we need to use Update handler instead of Message handler
	bh.Handle(b.textMessageHandler, th.Or(th.AnyMessageWithText(), AnyMessageWithPhoto()))
	logger.Debug("message handlers registered")

	logger.Info("starting bot handler")
	if err := bh.Start(); err != nil {
		logger.Error("cannot start bot handler", "error", err)
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
	logger := b.handlerLogger(ctx)

	if b.isMentionOfMe(message) || b.isReplyToMe(message) || b.isPrivateWithMe(message) {
		messageType := "private"
		if b.isMentionOfMe(message) {
			messageType = "mention"
		} else if b.isReplyToMe(message) {
			messageType = "reply"
		}
		logger.Info("processing message", "message_type", messageType)
		b.processMention(ctx, message)
	} else {
		logger.Debug("skipping message", "reason", "not a mention, reply, or private chat")
	}

	return nil
}

func (b *Bot) processMention(reqCtx *th.Context, message t.Message) {
	b.stats.Mention()

	chatID := tu.ID(message.Chat.ID)

	baseCtx := b.handlerContext(reqCtx)
	b.maybeSummarizeHistory(baseCtx, message.Chat.ID)
	logger := b.loggerFromContext(baseCtx)
	logger.Info("handling mention", "chat_id", message.Chat.ID)

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
			logger.Error("llm request timed out", "chat_id", message.Chat.ID, "error", err)
			timeout := b.cfg.ProcessingTimeout
			_, _ = b.api.SendMessage(baseCtx, b.reply(message, tu.Message(
				chatID,
				fmt.Sprintf("LLM request timed out after %s. Try again later.", timeout),
			)))

			return
		}

		logger.Error("cannot get reply from llm connector", "error", err)
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

	logger.Debug("sending llm reply", "reply_length", len(llmReply))

	sanitizedReply := b.sanitizer.Sanitize(llmReply)

	reply := tu.Message(
		chatID,
		sanitizedReply,
	).WithParseMode(t.ModeMarkdownV2)

	_, err = b.api.SendMessage(baseCtx, b.reply(message, reply))
	if err != nil {
		logger.Error("cannot send reply message", "error", err, "reply_length", len(sanitizedReply))
		sentry.CaptureException(err)

		b.trySendReplyError(baseCtx, message)

		return
	}

	b.saveBotReplyToHistory(baseCtx, message, llmReply)
}

func (b *Bot) summarizeHandler(ctx *th.Context, message t.Message) error {
	logger := b.handlerLogger(ctx)
	logger.Info("handling summarize command", "chat_id", message.Chat.ID)

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

	if !isValidAndAllowedURL(url) {
		logger.Warn("provided text is not a valid url")

		_, _ = ctx.Bot().SendMessage(ctx.Context(), b.reply(message, tu.Message(
			chatID,
			"URL is not valid.",
		)))

		return nil
	}

	var err error
	article, err := b.extractor.GetArticleFromURL(ctx.Context(), url)
	if err != nil {
		logger.Error("cannot retrieve article using extractor", "error", err, "url", url)
		sentry.CaptureException(err)

		_, _ = ctx.Bot().SendMessage(ctx.Context(), b.reply(message, tu.Message(
			chatID,
			"Failed to extract article content. Please check if the URL is correct.",
		)))

		return nil
	}

	if article.Text == "" {
		logger.Warn("article text is empty", "url", url)
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
			logger.Error("summarize request timed out", "chat_id", message.Chat.ID, "error", err)
			timeout := b.cfg.ProcessingTimeout
			_, _ = ctx.Bot().SendMessage(ctx.Context(), b.reply(message, tu.Message(
				chatID,
				fmt.Sprintf("LLM request timed out after %s. Try again later.", timeout),
			)))

			return nil
		}

		logger.Error("cannot get summarize reply from llm connector", "error", err)
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

	logger.Debug("sending summarize reply", "reply_length", len(summarizeReply))

	footerURL := b.sanitizer.EscapeURL(article.URL)
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
		"Hey!\r\n"+
			"Check out /help to learn how to use this bot.",
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
		logger.Error("cannot send reset confirmation", "error", err)
		sentry.CaptureException(err)

		b.trySendReplyError(ctx.Context(), message)
	}

	return nil
}
