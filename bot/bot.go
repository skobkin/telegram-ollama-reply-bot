package bot

import (
	"errors"
	"log/slog"
	"strconv"
	"strings"
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
)

const TELEGRAM_CHAR_LIMIT = 4096

type BotInfo struct {
	Id       int64
	Username string
	Name     string
}

type Bot struct {
	api       *telego.Bot
	llm       *llm.LlmConnector
	extractor extractor.Extractor
	stats     *stats.Stats
	models    ModelSelection
	history   map[int64]*MessageHistory
	profile   BotInfo

	markdownV1Replacer *strings.Replacer
}

func NewBot(
	api *telego.Bot,
	llm *llm.LlmConnector,
	extractor extractor.Extractor,
	models ModelSelection,
) *Bot {
	return &Bot{
		api:       api,
		llm:       llm,
		extractor: extractor,
		stats:     stats.NewStats(),
		models:    models,
		history:   make(map[int64]*MessageHistory),
		profile:   BotInfo{0, "", ""},

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
	botUser, err := b.api.GetMe()
	if err != nil {
		slog.Error("bot: Cannot retrieve api user", "error", err)
		sentry.CaptureException(err)

		return ErrGetMe
	}

	slog.Info("bot: Running api as", "id", botUser.ID, "username", botUser.Username, "name", botUser.FirstName, "is_bot", botUser.IsBot)
	sentry.AddBreadcrumb(&sentry.Breadcrumb{
		Category: "telegram-api",
		Message:  "Bot ID: " + strconv.FormatInt(botUser.ID, 10),
		Level:    sentry.LevelInfo,
	})

	b.profile = BotInfo{
		Id:       botUser.ID,
		Username: botUser.Username,
		Name:     botUser.FirstName,
	}

	updates, err := b.api.UpdatesViaLongPolling(nil)
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

	defer bh.Stop()
	defer b.api.StopLongPolling()

	// Middlewares
	bh.Use(b.chatHistory)
	bh.Use(b.chatTypeStatsCounter)

	// Command handlers
	bh.Handle(b.startHandler, th.CommandEqual("start"))
	bh.Handle(b.summarizeHandler, th.Or(th.CommandEqual("summarize"), th.CommandEqual("s")))
	bh.Handle(b.statsHandler, th.CommandEqual("stats"))
	bh.Handle(b.helpHandler, th.CommandEqual("help"))
	bh.Handle(b.textMessageHandler, th.AnyMessageWithText())

	bh.Start()

	return nil
}

func (b *Bot) textMessageHandler(bot *telego.Bot, update telego.Update) {
	slog.Debug("bot: /any-message")

	message := update.Message

	switch {
	// Mentions
	case b.isMentionOfMe(update):
		slog.Info("bot: /any-message", "type", "mention")
		b.processMention(message)
	// Replies
	case b.isReplyToMe(update):
		slog.Info("bot: /any-message", "type", "reply")
		b.processMention(message)
	// Private chat
	case b.isPrivateWithMe(update):
		slog.Info("bot: /any-message", "type", "private")
		b.processMention(message)
	default:
		slog.Debug("bot: /any-message", "info", "MessageData is not mention, reply or private chat. Skipping.")
	}
}

func (b *Bot) processMention(message *telego.Message) {
	b.stats.Mention()

	slog.Info("bot: /mention", "chat", message.Chat.ID)

	chatID := tu.ID(message.Chat.ID)

	b.sendTyping(chatID)

	requestContext := b.createLlmRequestContextFromMessage(message)

	userMessageData := tgUserMessageToMessageData(message, true)

	llmReply, err := b.llm.HandleChatMessage(
		messageDataToLlmMessage(userMessageData),
		b.models.TextRequestModel,
		requestContext,
	)
	if err != nil {
		slog.Error("bot: Cannot get reply from LLM connector")
		sentry.CaptureException(err)

		_, _ = b.api.SendMessage(b.reply(message, tu.Message(
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

	_, err = b.api.SendMessage(b.reply(message, reply))
	if err != nil {
		slog.Error("bot: Can't send reply message", "error", err)
		sentry.CaptureException(err)

		b.trySendReplyError(message)

		return
	}

	b.saveBotReplyToHistory(message, llmReply)
}

func (b *Bot) summarizeHandler(bot *telego.Bot, update telego.Update) {
	slog.Info("bot: /summarize", "message-text", update.Message.Text)

	b.stats.SummarizeRequest()

	chatID := tu.ID(update.Message.Chat.ID)

	b.sendTyping(chatID)

	args := strings.SplitN(update.Message.Text, " ", 3)
	argsCount := len(args)

	if argsCount < 2 {
		_, _ = bot.SendMessage(tu.Message(
			tu.ID(update.Message.Chat.ID),
			"Usage: /summarize <link>\r\n\r\n"+
				"Example:\r\n"+
				"/summarize https://kernel.org/get-notifications-for-your-patches.html",
		))

		return
	}

	url := args[1]
	additionalInstructions := ""
	if argsCount == 3 {
		additionalInstructions = args[2]
	}

	if !isValidAndAllowedUrl(url) {
		slog.Error("bot: Provided text is not a valid URL", "text", url)

		_, _ = bot.SendMessage(b.reply(update.Message, tu.Message(
			chatID,
			"URL is not valid.",
		)))

		return
	}

	article, err := b.extractor.GetArticleFromUrl(url)
	if err != nil {
		slog.Error("bot: Cannot retrieve an article using extractor", "error", err)
		sentry.CaptureException(err)

		_, _ = bot.SendMessage(b.reply(update.Message, tu.Message(
			chatID,
			"Failed to extract article content. Please check if the URL is correct.",
		)))

		return
	}

	if article.Text == "" {
		slog.Error("bot: Article text is empty", "url", url)
		sentry.CaptureMessage("Article text is empty")

		_, _ = bot.SendMessage(b.reply(update.Message, tu.Message(
			chatID,
			"No text extracted from the article. This resource is not supported at the moment.",
		)))

		return
	}

	llmReply, err := b.llm.Summarize(article.Text, b.models.SummarizeModel, additionalInstructions)
	if err != nil {
		slog.Error("bot: Cannot get reply from LLM connector")
		sentry.CaptureException(err)

		_, _ = b.api.SendMessage(b.reply(update.Message, tu.Message(
			chatID,
			"LLM request error. Try again later.",
		)))

		return
	}

	slog.Debug("bot: Got completion. Going to send reply.", "llm-completion", llmReply)

	footer := "\n\n[src](" + article.Url + ")"

	replyMarkdown := cropToMaxLengthMarkdownV2(b.escapeMarkdownV2Symbols(llmReply), TELEGRAM_CHAR_LIMIT-len(footer)) +
		footer

	message := tu.Message(
		chatID,
		replyMarkdown,
	).WithParseMode("MarkdownV2")

	_, err = bot.SendMessage(b.reply(update.Message, message))

	if err != nil {
		slog.Error("bot: Can't send reply message", "error", err)
		sentry.CaptureException(err)

		b.trySendReplyError(update.Message)
	}

	b.saveBotReplyToHistory(update.Message, replyMarkdown)
}

func (b *Bot) helpHandler(bot *telego.Bot, update telego.Update) {
	slog.Info("bot: /help")

	chatID := tu.ID(update.Message.Chat.ID)

	b.sendTyping(chatID)

	_, err := bot.SendMessage(b.reply(update.Message, tu.Messagef(
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

		b.trySendReplyError(update.Message)
	}
}

func (b *Bot) startHandler(bot *telego.Bot, update telego.Update) {
	slog.Info("bot: /start")

	chatID := tu.ID(update.Message.Chat.ID)

	b.sendTyping(chatID)

	_, err := bot.SendMessage(b.reply(update.Message, tu.Message(
		chatID,
		"Hey!\r\n"+
			"Check out /help to learn how to use this bot.",
	)))
	if err != nil {
		slog.Error("bot: Cannot send a message", "error", err)
		sentry.CaptureException(err)

		b.trySendReplyError(update.Message)
	}
}

func (b *Bot) statsHandler(bot *telego.Bot, update telego.Update) {
	slog.Info("bot: /stats")

	chatID := tu.ID(update.Message.Chat.ID)

	b.sendTyping(chatID)

	_, err := bot.SendMessage(b.reply(update.Message, tu.Message(
		chatID,
		"Current bot stats:\r\n"+
			"```json\r\n"+
			b.stats.String()+"\r\n"+
			"```",
	)).WithParseMode("Markdown"))
	if err != nil {
		slog.Error("bot: Cannot send a message", "error", err)
		sentry.CaptureException(err)

		b.trySendReplyError(update.Message)
	}
}

func (b *Bot) escapeMarkdownV1Symbols(input string) string {
	return b.markdownV1Replacer.Replace(input)
}

func (b *Bot) escapeMarkdownV2Symbols(input string) string {
	specialChars := "_*[]()~`>#+-=|{}.!"
	var escaped strings.Builder

	for _, char := range input {
		if strings.ContainsRune(specialChars, char) {
			escaped.WriteRune('\\')
		}
		escaped.WriteRune(char)
	}

	return escaped.String()
}
