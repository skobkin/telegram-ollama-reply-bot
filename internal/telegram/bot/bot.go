package bot

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"

	"telegram-ollama-reply-bot/internal/adminconfig"
	"telegram-ollama-reply-bot/internal/chatreply"
	"telegram-ollama-reply-bot/internal/config"
	"telegram-ollama-reply-bot/internal/content/extractor"
	"telegram-ollama-reply-bot/internal/llm"
	"telegram-ollama-reply-bot/internal/llmcontext"
	"telegram-ollama-reply-bot/internal/state"
	"telegram-ollama-reply-bot/internal/support/markdown"

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
	stats      state.StatsStore
	history    state.ConversationStore
	me         botInfo
	cfg        config.BotConfig
	ctx        context.Context
	imageCache state.ImageStore
	replyCtx   *llmcontext.ReplyBuilder
	logger     *slog.Logger
	admin      *adminconfig.Service
	replier    *chatreply.Service
}

func NewBot(
	ctx context.Context,
	api *t.Bot,
	llm *llm.Service,
	extractor extractor.Extractor,
	sanitizer markdown.Sanitizer,
	history state.ConversationStore,
	imageCache state.ImageStore,
	stats state.StatsStore,
	cfg config.BotConfig,
	admin *adminconfig.Service,
	replier *chatreply.Service,
	logger *slog.Logger,
) *Bot {
	if history == nil {
		panic("history store is required")
	}
	if imageCache == nil {
		panic("image store is required")
	}
	if stats == nil {
		panic("stats store is required")
	}
	if replier == nil {
		panic("chat replier is required")
	}

	bot := &Bot{
		api:        api,
		llm:        llm,
		extractor:  extractor,
		sanitizer:  sanitizer,
		stats:      stats,
		history:    history,
		me:         botInfo{},
		cfg:        cfg,
		ctx:        ctx,
		imageCache: imageCache,
		logger:     logger,
		admin:      admin,
		replier:    replier,
	}

	bot.replyCtx = llmcontext.NewReplyBuilder(history, bot.hydrateMessagesWithImageDescriptions, cfg.UncompressedHistoryLimit)

	return bot
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

	logger.Debug("registering message handlers")
	b.registerMiddlewares(bh)
	b.registerMessageHandlers(bh)
	logger.Debug("message handlers registered")

	logger.Info("starting bot handler")
	if err := bh.Start(); err != nil {
		logger.Error("cannot start bot handler", "error", err)
		sentry.CaptureException(err)

		return ErrHandlerStart
	}

	return nil
}

func (b *Bot) registerMiddlewares(bh *th.BotHandler) {
	// Request and access control.
	bh.Use(b.requestLogger)
	bh.Use(b.collectChatCatalog)
	bh.Use(b.accessGate)

	// Ephemeral state.
	bh.Use(b.chatHistory)
	bh.Use(b.chatTypeStatsCounter)
}

func (b *Bot) registerMessageHandlers(bh *th.BotHandler) {
	commandForMe := b.commandForThisBot()

	// User-facing commands.
	bh.HandleMessage(b.startHandler, th.And(commandForMe, th.CommandEqual("start")))
	bh.HandleMessage(b.summarizeHandler, th.And(commandForMe, th.Or(th.CommandEqual("summarize"), th.CommandEqual("s"))))
	bh.HandleMessage(b.statsHandler, th.And(commandForMe, th.CommandEqual("stats")))
	bh.HandleMessage(b.helpHandler, th.And(commandForMe, th.CommandEqual("help")))
	bh.HandleMessage(b.resetHandler, th.And(commandForMe, th.CommandEqual("reset")))

	// Admin DM commands.
	bh.HandleMessage(b.adminHelpHandler, th.And(commandForMe, th.CommandEqual("admin_help")))
	bh.HandleMessage(b.chatListHandler, th.And(commandForMe, th.CommandEqual("chat_list")))
	bh.HandleMessage(b.configFieldsGlobalHandler, th.And(commandForMe, th.CommandEqual("config_fields_global")))
	bh.HandleMessage(b.configFieldsChatHandler, th.And(commandForMe, th.CommandEqual("config_fields_chat")))
	bh.HandleMessage(b.promptFeaturesHandler, th.And(commandForMe, th.CommandEqual("prompt_features")))
	bh.HandleMessage(b.configShowHandler, th.And(commandForMe, th.CommandEqual("config_show")))
	bh.HandleMessage(b.configSetGlobalHandler, th.And(commandForMe, th.CommandEqual("config_set_global")))
	bh.HandleMessage(b.configSetChatHandler, th.And(commandForMe, th.CommandEqual("config_set_chat")))
	bh.HandleMessage(b.configClearChatHandler, th.And(commandForMe, th.CommandEqual("config_clear_chat")))
	bh.HandleMessage(b.promptShowHandler, th.And(commandForMe, th.CommandEqual("prompt_show")))
	bh.HandleMessage(b.promptSetGlobalHandler, th.And(commandForMe, th.CommandEqual("prompt_set_global")))
	bh.HandleMessage(b.promptSetChatHandler, th.And(commandForMe, th.CommandEqual("prompt_set_chat")))
	bh.HandleMessage(b.promptClearChatHandler, th.And(commandForMe, th.CommandEqual("prompt_clear_chat")))
	bh.HandleMessage(b.whitelistAddHandler, th.And(commandForMe, th.CommandEqual("whitelist_add")))
	bh.HandleMessage(b.whitelistRemoveHandler, th.And(commandForMe, th.CommandEqual("whitelist_remove")))
	bh.HandleMessage(b.whitelistListHandler, th.And(commandForMe, th.CommandEqual("whitelist_list")))

	// Conversational messages and image posts.
	bh.Handle(b.textMessageHandler, th.Or(th.AnyMessageWithText(), AnyMessageWithPhoto()))
}

func (b *Bot) textMessageHandler(ctx *th.Context, update t.Update) error {
	if update.Message == nil {
		return nil
	}
	message := *update.Message
	logger := b.handlerLogger(ctx)

	if !b.shouldProcessChatMessage(ctx.Context(), message) {
		logger.Debug("skipping message", "reason", "interactivity mode")

		return nil
	}

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
	b.maybeSummarizeHistory(baseCtx, message)
	logger := b.loggerFromContext(baseCtx)
	logger.Info("handling mention", "chat_id", message.Chat.ID)

	// Get MessageData from the request context if available, otherwise create it on the fly
	userMessageData := b.getMessageDataFromRequestContextOrCreate(reqCtx, message, true)
	triggerKind := b.replyTriggerKind(message)

	var llmReply string
	var usage *llm.TokenUsage
	var err error

	err = b.runWithTimeout(baseCtx, chatID, func(ctx context.Context) error {
		var userCtx llmcontext.UserContext
		if message.From != nil {
			userCtx = llmcontext.UserContext{
				Username:  message.From.Username,
				FirstName: message.From.FirstName,
				LastName:  message.From.LastName,
				IsPremium: message.From.IsPremium,
			}
		}

		requestContext := b.replyCtx.BuildReplyContext(ctx, llmcontext.ReplyInput{
			Chat: llmcontext.ChatContext{
				Title: message.Chat.Title,
				Type:  message.Chat.Type,
			},
			User:           userCtx,
			Scope:          scopeFromMessage(message),
			CurrentMessage: userMessageData,
			Trigger:        triggerKind,
		})

		llmCtx, cancel := b.withProcessingDeadline(ctx)
		defer cancel()

		llmReply, usage, err = b.replier.HandleChatMessage(llmCtx, chatreply.Request{
			Scope:          scopeFromMessage(message),
			PromptScope:    llm.PromptScope{ChatID: message.Chat.ID},
			ReplyContext:   requestContext,
			RequestMessage: userMessageData,
		})

		return err
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

func (b *Bot) replyTriggerKind(message t.Message) llmcontext.TriggerKind {
	switch {
	case b.isMentionOfMe(message):
		return llmcontext.TriggerMention
	case b.isReplyToMe(message):
		return llmcontext.TriggerReply
	default:
		return llmcontext.TriggerPrivate
	}
}
