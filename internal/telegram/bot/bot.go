package bot

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"telegram-ollama-reply-bot/internal/adminconfig"
	"telegram-ollama-reply-bot/internal/config"
	"telegram-ollama-reply-bot/internal/content/extractor"
	"telegram-ollama-reply-bot/internal/execution"
	"telegram-ollama-reply-bot/internal/llm"
	"telegram-ollama-reply-bot/internal/llmcontext"
	"telegram-ollama-reply-bot/internal/state"
	"telegram-ollama-reply-bot/internal/support/markdown"
	"telegram-ollama-reply-bot/internal/tooluse"
	"telegram-ollama-reply-bot/internal/vision"

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

// longPollingTimeoutSeconds is the explicit GetUpdates long-polling timeout
// required when update params are non-nil (telego's default applies only to
// nil params).
const longPollingTimeoutSeconds = 8

// ImageRecognitionSettings carries the image-recognition switch and the
// client-side download/processing bound into the bot.
type ImageRecognitionSettings struct {
	Enabled       bool
	MaxImageBytes int64
}

// imageDescriber decouples the bot from the vision service so tests can fake
// recognition. Analyze is the model-prompted re-recognition path.
type imageDescriber interface {
	Describe(ctx context.Context, imageData []byte) (vision.Recognition, error)
	Analyze(ctx context.Context, imageData []byte, prompt string) (vision.Recognition, error)
}

// AdminUI is the interactive admin interface hook. The adapter registers its
// routes at the isolation point before the ephemeral-state middlewares, so
// every administrative update (commands, callbacks, correlated replies,
// chat-sharing events) terminates before conversation history and request
// statistics. SyncCommandMenus pushes the Telegram command menus (called once
// from Run after GetMe resolves the bot identity).
type AdminUI interface {
	RegisterRoutes(bh *th.BotHandler, api *t.Bot, botUserID int64, commandForMe th.Predicate)
	SyncCommandMenus(ctx context.Context)
}

type Bot struct {
	api              *t.Bot
	llm              llmSummarizer
	vision           imageDescriber
	extractor        extractor.Extractor
	sanitizer        markdown.Sanitizer
	stats            state.StatsStore
	history          state.ConversationStore
	me               botInfo
	cfg              config.BotConfig
	ctx              context.Context
	imageCache       state.ImageStore
	replyCtx         *llmcontext.ReplyBuilder
	logger           *slog.Logger
	admin            *adminconfig.Service
	adminUI          AdminUI
	tools            *tooluse.Runtime
	imageRecognition ImageRecognitionSettings
	autoSummaryURLs  *autoSummaryTracker
	// previews manages live tool-progress previews; nil when no execution
	// dispatcher was wired, in which case previews stay off entirely.
	previews *previewManager
	// events publishes observational execution events from the reaction
	// ingress; nil when no dispatcher was wired, making publication a
	// no-op. Reaction history never depends on event delivery.
	events execution.Dispatcher
	// pendingReactions holds reaction updates that arrived while their target
	// reply's send response was still in flight; replayed on history
	// insertion. The zero value is usable.
	pendingReactions pendingReactionBuffer
}

func NewBot(
	ctx context.Context,
	api *t.Bot,
	llm *llm.Service,
	vision imageDescriber,
	extractor extractor.Extractor,
	sanitizer markdown.Sanitizer,
	history state.ConversationStore,
	imageCache state.ImageStore,
	stats state.StatsStore,
	cfg config.BotConfig,
	admin *adminconfig.Service,
	tools *tooluse.Runtime,
	logger *slog.Logger,
	imageRecognition ImageRecognitionSettings,
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
	if tools == nil {
		panic("tool runtime is required")
	}

	bot := &Bot{
		api:              api,
		llm:              llm,
		vision:           vision,
		extractor:        extractor,
		sanitizer:        sanitizer,
		stats:            stats,
		history:          history,
		me:               botInfo{},
		cfg:              cfg,
		ctx:              ctx,
		imageCache:       imageCache,
		logger:           logger,
		admin:            admin,
		tools:            tools,
		imageRecognition: imageRecognition,
		autoSummaryURLs:  newAutoSummaryTracker(autoSummaryMaxTrackedURLsPerChat),
	}

	// The summary tail limit admits the normal unsummarized window plus room
	// for messages arriving mid-generation, so it only binds during partial
	// summary rebuilds; see ReplyBuilder.summaryTailLimit.
	bot.replyCtx = llmcontext.NewReplyBuilder(history, bot.hydrateMessagesWithImageDescriptions, cfg.UncompressedHistoryLimit, cfg.UncompressedHistoryLimit+2*cfg.HistorySummaryThreshold)

	return bot
}

// SetAdminUI wires the interactive admin UI adapter (app wiring; the hook is
// inert until then).
func (b *Bot) SetAdminUI(ui AdminUI) {
	b.adminUI = ui
}

// executionEventBus is the dispatcher contract the bot wiring needs:
// subscribing the live preview manager and publishing execution events
// from the reaction ingress. The concrete dispatcher from
// internal/execution satisfies it.
type executionEventBus interface {
	execution.Dispatcher
	execution.Subscriber
}

// SetExecutionDispatcher wires the execution event dispatcher: it
// subscribes the live preview manager and arms observational reaction
// event publication. It returns the manager's stop function (nil without
// a dispatcher): call it at shutdown before stopping the dispatcher
// itself, so preview workers are joined before any other event consumer
// goes away. Passing nil keeps previews off and reaction publication
// disabled.
func (b *Bot) SetExecutionDispatcher(dispatcher executionEventBus) func() {
	if dispatcher == nil {
		return nil
	}

	manager := newPreviewManager(b.ctx, b.api, b.logger, b.sendTypingUntil)
	manager.unsubscribe = dispatcher.Subscribe(manager)
	b.events = dispatcher
	b.previews = manager

	return manager.Stop
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

	logger.Info("telegram api connected",
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

	// The update set is enumerated explicitly: with no allowed_updates list
	// Telegram uses its default set, which excludes message_reaction. Only
	// consumed types are requested — anything else was already ignored by
	// the route table. With non-nil params the timeout must be set
	// explicitly (telego applies its default only to nil params).
	updates, err := b.api.UpdatesViaLongPolling(b.ctx, &t.GetUpdatesParams{
		Timeout:        longPollingTimeoutSeconds,
		AllowedUpdates: []string{t.MessageUpdates, t.CallbackQueryUpdates, t.MessageReactionUpdates},
	})
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

	logger.Debug("registering routes")
	b.registerRoutes(bh)
	logger.Debug("routes registered")

	if b.adminUI != nil {
		logger.Debug("syncing command menus")
		b.adminUI.SyncCommandMenus(b.ctx)
	}

	logger.Debug("starting bot handler")
	if err := bh.Start(); err != nil {
		logger.Error("cannot start bot handler", "error", err)
		sentry.CaptureException(err)

		return ErrHandlerStart
	}

	return nil
}

// registerRoutes wires the whole update pipeline in one explicit order.
// Route order is load-bearing: middlewares and handlers share one ordered
// list and the first matching terminal route stops the chain.
func (b *Bot) registerRoutes(bh *th.BotHandler) {
	commandForMe := b.commandForThisBot()

	// Request and access control. The DM restriction gate runs before the
	// catalog so denied private messages leave no trace at all.
	bh.Use(b.requestLogger)
	bh.Use(b.dmAdminsOnlyGate)
	bh.Use(b.collectChatCatalog)
	bh.Use(b.accessGate)

	// The interactive admin UI adapter registers its routes here (private
	// admin commands, callbacks, correlated replies, chat sharing) so every
	// administrative update terminates before the ephemeral-state
	// middlewares. Wired in Run when the adapter is present.
	if b.adminUI != nil {
		b.adminUI.RegisterRoutes(bh, b.api, b.me.ID, commandForMe)
	}

	// Administrative commands: same handlers and authorization as before,
	// but registered before the ephemeral-state middlewares so they no
	// longer land in conversation history or ordinary request statistics.
	bh.HandleMessage(b.statsHandler, th.And(commandForMe, th.CommandEqual("stats")))
	bh.HandleMessage(b.resetHandler, th.And(commandForMe, th.CommandEqual("reset")))

	// Unknown commands are dropped before the conversation pipeline: a
	// typo'd command or a removed legacy admin command must never reach the
	// LLM, the history, or the request statistics.
	bh.HandleMessage(b.unknownCommandHandler, th.And(commandForMe, b.notAUserCommand()))

	// Ephemeral state.
	bh.Use(b.chatHistory)
	bh.Use(b.chatTypeStatsCounter)

	// Reactions on the bot's own messages. Reaction updates carry no Message
	// payload, so the shared middlewares above all no-op for them (including
	// the access gate) — the handler enforces the gate itself. Generic route
	// (instead of the typed HandleMessageReaction) so the envelope's
	// sequential UpdateID reaches the store for stale-update rejection.
	bh.Handle(b.messageReactionHandler, th.AnyMessageReaction())

	// User-facing commands.
	bh.HandleMessage(b.startHandler, th.And(commandForMe, th.CommandEqual("start")))
	bh.HandleMessage(b.summarizeHandler, th.And(commandForMe, th.Or(th.CommandEqual("summarize"), th.CommandEqual("s"))))
	bh.HandleMessage(b.helpHandler, th.And(commandForMe, th.CommandEqual("help")))

	// Conversational messages and image posts.
	bh.Handle(b.textMessageHandler, th.Or(th.AnyMessageWithText(), AnyMessageWithPhoto()))
}

func (b *Bot) textMessageHandler(ctx *th.Context, update t.Update) error {
	if update.Message == nil {
		return nil
	}
	message := *update.Message
	logger := b.handlerLogger(ctx)

	resolved, ok := b.resolveChatConfig(ctx.Context(), message)
	if !ok {
		logger.Debug("skipping message", "reason", "chat config resolution failed")

		return nil
	}

	conversational := b.isMentionOfMe(message) || b.isReplyToMe(message) || b.isPrivateWithMe(message)

	// The auto-summarizer is independent of the interactivity gate: it is
	// its own per-chat opt-in, so it must see messages that the interactivity
	// mode would ignore. Messages the bot already answers conversationally
	// are excluded to avoid double answers for the same link.
	if !conversational {
		b.maybeAutoSummarize(ctx, message, resolved)
	}

	if !b.shouldProcessChatMessage(resolved, message) {
		logger.Debug("skipping message", "reason", "interactivity mode")

		return nil
	}

	if conversational {
		messageType := "private"
		if b.isMentionOfMe(message) {
			messageType = "mention"
		} else if b.isReplyToMe(message) {
			messageType = "reply"
		}
		logger.Info("processing message", "message_type", messageType)
		b.processMention(ctx, message, resolved)
	} else {
		logger.Debug("skipping message", "reason", "not a mention, reply, or private chat")
	}

	return nil
}

func (b *Bot) processMention(reqCtx *th.Context, message t.Message, resolved adminconfig.ResolvedChatConfig) {
	b.stats.Mention()

	chatID := tu.ID(message.Chat.ID)

	baseCtx := b.handlerContext(reqCtx)

	// The preview starts before preprocessing so the neutral indicator
	// covers the entire run, history summarization included. It returns nil
	// (and everything below no-ops) when previews are off.
	run := b.previews.RunFor(RunInfo{
		RunID:            execution.NewRunID(),
		ChatID:           message.Chat.ID,
		TopicID:          message.MessageThreadID,
		TriggerMessageID: message.MessageID,
		ChatType:         message.Chat.Type,
		Mode:             resolved.PreviewMode,
		KeepToolHistory:  resolved.KeepToolHistory,
	})
	defer run.Close() // safety net: state cleanup on every exit path

	b.maybeSummarizeHistory(baseCtx, message)
	logger := b.loggerFromContext(baseCtx)
	logger.Debug("handling mention")

	// Get MessageData from the request context if available, otherwise create it on the fly
	userMessageData := b.getMessageDataFromRequestContextOrCreate(reqCtx, message, true)
	triggerKind := b.replyTriggerKind(message)

	var llmReply string
	var usage *llm.TokenUsage
	var toolCalls []tooluse.ToolCallRecord
	var err error

	err = b.runWithPreviewTimeout(baseCtx, chatID, run, func(ctx context.Context) error {
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

		result, runErr := b.tools.ReplyWithTools(llmCtx, tooluse.ChatRequest{
			Scope:          scopeFromMessage(message),
			PromptScope:    llm.PromptScope{ChatID: message.Chat.ID},
			ReplyContext:   requestContext,
			RequestMessage: userMessageData,
			RunID:          run.ID(),
		})
		llmReply, usage, err = result.Reply, result.Usage, runErr
		toolCalls = result.ToolCalls

		return runErr
	})
	if err != nil {
		run.Abort()

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

	if strings.TrimSpace(llmReply) == "" {
		logger.Debug("llm interaction completed without final text reply")
		run.CompleteNoText(toolCalls)

		return
	}

	// Join all preview writes before delivery: a 429 landing on a
	// still-in-flight preview call closes the chat's wire window only
	// through the gate, so it must be harvested into the gate before
	// delivery looks at it — otherwise the answer walks into a window that
	// had already opened. Quiescing also keeps a slow private draft from
	// landing after the answer and resurrecting the working preview below
	// it; group previews freeze at their last state until FinalDelivered
	// deletes them. With previews off this is a no-op.
	run.Quiesce()

	// The gate itself is consulted inside deliverFinalReply, under its
	// bounded delivery budget: an outstanding backoff is waited out there,
	// and a 429 the send draws is absorbed, waited, and redelivered by the
	// same helper — a preliminary wait here would run unbounded against a
	// backoff larger than the budget.
	sent, err := b.deliverFinalReply(baseCtx, message, llmReply, toolCalls, resolved)
	if err != nil {
		run.Abort()

		logger.Error("cannot send reply message", "error", err, "reply_length", len(llmReply))
		sentry.CaptureException(err)

		b.trySendReplyError(baseCtx, message)

		return
	}

	run.FinalDelivered()

	replyMessageID := 0
	if sent != nil {
		replyMessageID = sent.MessageID
	}

	b.saveBotReplyToHistory(baseCtx, message, llmReply, replyMessageID)
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
