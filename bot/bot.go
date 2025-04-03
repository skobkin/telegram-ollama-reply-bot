package bot

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"telegram-ollama-reply-bot/config"
	"telegram-ollama-reply-bot/extractor"
	"telegram-ollama-reply-bot/llm"
	"telegram-ollama-reply-bot/stats"

	"github.com/getsentry/sentry-go"
	"github.com/mymmrac/telego"
	th "github.com/mymmrac/telego/telegohandler"
	tu "github.com/mymmrac/telego/telegoutil"
)

var (
	ErrGetMe          = errors.New("cannot retrieve api user")
	ErrUpdatesChannel = errors.New("cannot get updates channel")
	ErrHandlerInit    = errors.New("cannot initialize handler")
	ErrHandlerStart   = errors.New("cannot start bot handler")
)

const TELEGRAM_CHAR_LIMIT = 4096

type Info struct {
	Id       int64
	Username string
	Name     string
}

type Bot struct {
	api       *telego.Bot
	llm       *llm.LlmConnector
	extractor extractor.Extractor
	stats     *stats.Stats
	history   map[int64]*MessageHistory
	profile   Info
	cfg       config.BotConfig
	ctx       context.Context

	markdownV1Replacer *strings.Replacer
}

func NewBot(
	api *telego.Bot,
	llm *llm.LlmConnector,
	extractor extractor.Extractor,
	cfg config.BotConfig,
	ctx context.Context,
) *Bot {
	return &Bot{
		api:       api,
		llm:       llm,
		extractor: extractor,
		stats:     stats.NewStats(),
		history:   make(map[int64]*MessageHistory),
		profile:   Info{0, "", ""},
		cfg:       cfg,
		ctx:       ctx,

		markdownV1Replacer: strings.NewReplacer(
			// https://core.telegram.org/bots/api#markdown-style
			"_", "\\_",
			//"*", "\\*",
			//"`", "\\`",
			//"[", "\\[",
		),
	}
}

func (b *Bot) Run() error {
	botUser, err := b.api.GetMe(b.ctx)
	if err != nil {
		slog.Error("bot: Cannot retrieve api user", "error", err)
		sentry.CaptureException(err)

		return ErrGetMe
	}

	slog.Info("bot: Running api as",
		"id", botUser.ID,
		"username", botUser.Username,
		"name", botUser.FirstName,
		"is_bot", botUser.IsBot)
	sentry.AddBreadcrumb(&sentry.Breadcrumb{
		Category: "telegram-api",
		Message:  "Bot ID: " + strconv.FormatInt(botUser.ID, 10),
		Level:    sentry.LevelInfo,
	})

	b.profile = Info{
		Id:       botUser.ID,
		Username: botUser.Username,
		Name:     botUser.FirstName,
	}

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
	bh.HandleMessage(b.startHandler, th.CommandEqual("start"))
	bh.HandleMessage(b.summarizeHandler, th.Or(th.CommandEqual("summarize"), th.CommandEqual("s")))
	bh.HandleMessage(b.statsHandler, th.CommandEqual("stats"))
	bh.HandleMessage(b.helpHandler, th.CommandEqual("help"))
	bh.HandleMessage(b.resetHandler, th.CommandEqual("reset"))
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

func (b *Bot) textMessageHandler(ctx *th.Context, update telego.Update) error {
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

func (b *Bot) processMention(reqCtx *th.Context, message telego.Message) {
	b.stats.Mention()

	slog.Info("bot: /mention", "chat", message.Chat.ID)

	chatID := tu.ID(message.Chat.ID)

	b.sendTyping(chatID)

	requestContext := b.createLlmRequestContextFromMessage(message)

	// Get MessageData from the request context if available, otherwise create it on the fly
	userMessageData := b.getMessageDataFromRequestContextOrCreate(reqCtx, message, true)

	llmReply, err := b.llm.HandleChatMessage(
		messageDataToLlmMessage(userMessageData),
		requestContext,
	)
	if err != nil {
		slog.Error("bot: Cannot get reply from LLM connector")
		sentry.CaptureException(err)

		_, _ = b.api.SendMessage(b.ctx, b.reply(message, tu.Message(
			chatID,
			"LLM request error. Try again later.",
		)))

		return
	}

	slog.Debug("bot: Got completion. Going to send.", "llm-completion", llmReply)

	reply := tu.Message(
		chatID,
		b.escapeMarkdownV1Symbols(llmReply),
	).WithParseMode("Markdown")

	_, err = b.api.SendMessage(b.ctx, b.reply(message, reply))
	if err != nil {
		slog.Error("bot: Can't send reply message", "error", err)
		sentry.CaptureException(err)

		b.trySendReplyError(message)

		return
	}

	b.saveBotReplyToHistory(message, llmReply)
}

func (b *Bot) summarizeHandler(ctx *th.Context, message telego.Message) error {
	slog.Info("bot: /summarize", "message-text", message.Text)

	b.stats.SummarizeRequest()

	chatID := tu.ID(message.Chat.ID)

	b.sendTyping(chatID)

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

	url := args[1]
	additionalInstructions := ""
	if argsCount == 3 {
		additionalInstructions = args[2]
	}

	if !isValidAndAllowedUrl(url) {
		slog.Error("bot: Provided text is not a valid URL", "text", url)

		_, _ = ctx.Bot().SendMessage(ctx.Context(), b.reply(message, tu.Message(
			chatID,
			"URL is not valid.",
		)))

		return nil
	}

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

	llmReply, err := b.llm.Summarize(article.Text, additionalInstructions)
	if err != nil {
		slog.Error("bot: Cannot get reply from LLM connector")
		sentry.CaptureException(err)

		_, _ = ctx.Bot().SendMessage(ctx.Context(), b.reply(message, tu.Message(
			chatID,
			"LLM request error. Try again later.",
		)))

		return nil
	}

	slog.Debug("bot: Got completion. Going to send reply.", "llm-completion", llmReply)

	footer := "\n\n[src](" + article.Url + ")"

	replyMarkdown := cropToMaxLengthMarkdownV2(b.escapeMarkdownV2Symbols(llmReply), TELEGRAM_CHAR_LIMIT-len(footer)) +
		footer

	replyMessage := tu.Message(
		chatID,
		replyMarkdown,
	).WithParseMode("MarkdownV2")

	_, err = ctx.Bot().SendMessage(ctx.Context(), b.reply(message, replyMessage))

	if err != nil {
		slog.Error("bot: Can't send reply message", "error", err)
		sentry.CaptureException(err)

		b.trySendReplyError(message)
	}

	b.saveBotReplyToHistory(message, replyMarkdown)
	return nil
}

func (b *Bot) helpHandler(ctx *th.Context, message telego.Message) error {
	slog.Info("bot: /help")

	chatID := tu.ID(message.Chat.ID)

	b.sendTyping(chatID)

	_, err := ctx.Bot().SendMessage(ctx.Context(), b.reply(message, tu.Messagef(
		chatID,
		"Instructions:\r\n"+
			"/hey <text> - Ask something from LLM\r\n"+
			"/summarize <link> - Summarize text from the provided link\r\n"+
			"/s <link> - Shorter version\r\n"+
			"/help - Show this help\r\n\r\n"+
			"Mention bot or reply to it's message to communicate with it",
	)))
	if err != nil {
		slog.Error("bot: Cannot send a message", "error", err)
		sentry.CaptureException(err)

		b.trySendReplyError(message)
	}
	return nil
}

func (b *Bot) startHandler(ctx *th.Context, message telego.Message) error {
	slog.Info("bot: /start")

	chatID := tu.ID(message.Chat.ID)

	b.sendTyping(chatID)

	_, err := ctx.Bot().SendMessage(ctx.Context(), b.reply(message, tu.Message(
		chatID,
		"Hey!\r\n"+
			"Check out /help to learn how to use this bot.",
	)))
	if err != nil {
		slog.Error("bot: Cannot send a message", "error", err)
		sentry.CaptureException(err)

		b.trySendReplyError(message)
	}
	return nil
}

func (b *Bot) statsHandler(ctx *th.Context, message telego.Message) error {
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

	b.sendTyping(chatID)

	_, err := ctx.Bot().SendMessage(ctx.Context(), b.reply(message, tu.Message(
		chatID,
		"Current bot stats:\r\n"+
			"```json\r\n"+
			b.stats.String()+"\r\n"+
			"```",
	)).WithParseMode("Markdown"))
	if err != nil {
		slog.Error("bot: Cannot send a message", "error", err)
		sentry.CaptureException(err)

		b.trySendReplyError(message)
	}
	return nil
}

func (b *Bot) resetHandler(ctx *th.Context, message telego.Message) error {
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

	b.sendTyping(tu.ID(chatID))

	b.ResetChatHistory(chatID)
	b.stats.ChatHistoryReset()

	_, err := ctx.Bot().SendMessage(ctx.Context(), b.reply(message, tu.Message(
		tu.ID(chatID),
		"Okay, let's start fresh.",
	)))
	if err != nil {
		slog.Error("bot: Cannot send a message", "error", err)
		sentry.CaptureException(err)

		b.trySendReplyError(message)
	}
	return nil
}
