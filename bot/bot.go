package bot

import (
	"errors"
	"github.com/mymmrac/telego"
	th "github.com/mymmrac/telego/telegohandler"
	tu "github.com/mymmrac/telego/telegoutil"
	"log/slog"
	"strings"
	"telegram-ollama-reply-bot/extractor"
	"telegram-ollama-reply-bot/llm"
	"telegram-ollama-reply-bot/stats"
)

var (
	ErrGetMe          = errors.New("cannot retrieve api user")
	ErrUpdatesChannel = errors.New("cannot get updates channel")
	ErrHandlerInit    = errors.New("cannot initialize handler")
)

type Bot struct {
	api       *telego.Bot
	llm       *llm.LlmConnector
	extractor *extractor.Extractor
	stats     *stats.Stats

	markdownV1Replacer *strings.Replacer
}

func NewBot(api *telego.Bot, llm *llm.LlmConnector, extractor *extractor.Extractor) *Bot {
	return &Bot{
		api:       api,
		llm:       llm,
		extractor: extractor,
		stats:     stats.NewStats(),

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
		slog.Error("Cannot retrieve api user", "error", err)

		return ErrGetMe
	}

	slog.Info("Running api as", "id", botUser.ID, "username", botUser.Username, "name", botUser.FirstName, "is_bot", botUser.IsBot)

	updates, err := b.api.UpdatesViaLongPolling(nil)
	if err != nil {
		slog.Error("Cannot get update channel", "error", err)

		return ErrUpdatesChannel
	}

	bh, err := th.NewBotHandler(b.api, updates)
	if err != nil {
		slog.Error("Cannot initialize bot handler", "error", err)

		return ErrHandlerInit
	}

	defer bh.Stop()
	defer b.api.StopLongPolling()

	// Middlewares
	bh.Use(b.chatTypeStatsCounter)

	// Handlers
	bh.Handle(b.startHandler, th.CommandEqual("start"))
	bh.Handle(b.heyHandler, th.CommandEqual("hey"))
	bh.Handle(b.summarizeHandler, th.CommandEqual("summarize"))
	bh.Handle(b.statsHandler, th.CommandEqual("stats"))
	bh.Handle(b.helpHandler, th.CommandEqual("help"))

	bh.Start()

	return nil
}

func (b *Bot) heyHandler(bot *telego.Bot, update telego.Update) {
	slog.Info("/hey", "message-text", update.Message.Text)

	b.stats.HeyRequest()

	parts := strings.SplitN(update.Message.Text, " ", 2)
	userMessage := "Hey!"
	if len(parts) == 2 {
		userMessage = parts[1]
	}

	chatID := tu.ID(update.Message.Chat.ID)

	b.sendTyping(chatID)

	requestContext := b.createLlmRequestContext(update)

	llmReply, err := b.llm.HandleSingleRequest(userMessage, llm.ModelMistralUncensored, requestContext)
	if err != nil {
		slog.Error("Cannot get reply from LLM connector")

		_, _ = b.api.SendMessage(b.reply(update.Message, tu.Message(
			chatID,
			"LLM request error. Try again later.",
		)))

		return
	}

	slog.Debug("Got completion. Going to send.", "llm-reply", llmReply)

	message := tu.Message(
		chatID,
		b.escapeMarkdownV1Symbols(llmReply),
	).WithParseMode("Markdown")

	_, err = bot.SendMessage(b.reply(update.Message, message))
	if err != nil {
		slog.Error("Can't send reply message", "error", err)

		b.trySendReplyError(update.Message)
	}
}

func (b *Bot) summarizeHandler(bot *telego.Bot, update telego.Update) {
	slog.Info("/summarize", "message-text", update.Message.Text)

	b.stats.SummarizeRequest()

	chatID := tu.ID(update.Message.Chat.ID)

	b.sendTyping(chatID)

	args := strings.Split(update.Message.Text, " ")

	if len(args) < 2 {
		_, _ = bot.SendMessage(tu.Message(
			tu.ID(update.Message.Chat.ID),
			"Usage: /summarize <link>\r\n\r\n"+
				"Example:\r\n"+
				"/summarize https://kernel.org/get-notifications-for-your-patches.html",
		))

		return
	}

	if !isValidAndAllowedUrl(args[1]) {
		slog.Error("Provided text is not a valid URL", "text", args[1])

		_, _ = b.api.SendMessage(b.reply(update.Message, tu.Message(
			chatID,
			"URL is not valid.",
		)))

		return
	}

	article, err := b.extractor.GetArticleFromUrl(args[1])
	if err != nil {
		slog.Error("Cannot retrieve an article using extractor", "error", err)
	}

	llmReply, err := b.llm.Summarize(article.Text, llm.ModelMistralUncensored)
	if err != nil {
		slog.Error("Cannot get reply from LLM connector")

		_, _ = b.api.SendMessage(b.reply(update.Message, tu.Message(
			chatID,
			"LLM request error. Try again later.",
		)))

		return
	}

	slog.Debug("Got completion. Going to send.", "llm-reply", llmReply)

	message := tu.Message(
		chatID,
		b.escapeMarkdownV1Symbols(llmReply),
	).WithParseMode("Markdown")

	_, err = bot.SendMessage(b.reply(update.Message, message))

	if err != nil {
		slog.Error("Can't send reply message", "error", err)

		b.trySendReplyError(update.Message)
	}
}

func (b *Bot) helpHandler(bot *telego.Bot, update telego.Update) {
	slog.Info("/help")

	chatID := tu.ID(update.Message.Chat.ID)

	b.sendTyping(chatID)

	_, err := bot.SendMessage(b.reply(update.Message, tu.Messagef(
		chatID,
		"Instructions:\r\n"+
			"/hey <text> - Ask something from LLM\r\n"+
			"/summarize <link> - Summarize text from the provided link\r\n"+
			"/help - Show this help",
	)))
	if err != nil {
		slog.Error("Cannot send a message", "error", err)

		b.trySendReplyError(update.Message)
	}
}

func (b *Bot) startHandler(bot *telego.Bot, update telego.Update) {
	slog.Info("/start")

	chatID := tu.ID(update.Message.Chat.ID)

	b.sendTyping(chatID)

	_, err := bot.SendMessage(b.reply(update.Message, tu.Message(
		chatID,
		"Hey!\r\n"+
			"Check out /help to learn how to use this bot.",
	)))
	if err != nil {
		slog.Error("Cannot send a message", "error", err)

		b.trySendReplyError(update.Message)
	}
}

func (b *Bot) statsHandler(bot *telego.Bot, update telego.Update) {
	slog.Info("/stats")

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
		slog.Error("Cannot send a message", "error", err)

		b.trySendReplyError(update.Message)
	}
}

func (b *Bot) escapeMarkdownV1Symbols(input string) string {
	return b.markdownV1Replacer.Replace(input)
}
