package bot

import (
	"context"
	"errors"
	"sync"
	"time"

	"telegram-ollama-reply-bot/internal/adminconfig"
	"telegram-ollama-reply-bot/internal/interactivity"
	"telegram-ollama-reply-bot/internal/llm"
	"telegram-ollama-reply-bot/internal/llmcontext"
	"telegram-ollama-reply-bot/internal/state"

	"github.com/getsentry/sentry-go"
	t "github.com/mymmrac/telego"
)

// interactionDecider is the decision engine contract the proactive path
// needs; *interactivity.Engine is the production implementation.
type interactionDecider interface {
	Decide(ctx context.Context, input interactivity.DecisionInput) (interactivity.ActionDecision, error)
}

// proactiveDecisions holds the proactive wiring. The engine builder is an
// indirection: production constructs interactivity.NewEngine over the shared
// boundaries per operation, and the model resolver is not stored at all —
// each operation adapts its own settings snapshot (snapshotModels), so the
// decision and its resulting action share one resolution without a second
// settings read inside the engine.
type proactiveDecisions struct {
	engineFor func(resolved adminconfig.ResolvedChatConfig) interactionDecider
	history   interactivity.HistorySource
	// persona renders decision personas and captures the operation's
	// template body at snapshot time.
	persona interactivity.PersonaSource
	// budget caps the whole serialized decision request (composition limit).
	budget int64
}

// SetProactiveDecisions wires the proactive participation feature: eligible
// non-explicit group messages are evaluated by the interaction decision
// engine, and a positive decision runs the proactive reaction or reply turn.
// The bot contributes the history source from its own reply-context builder;
// the caller contributes the shared System One client, the persona renderer,
// and the request composition budget. It returns the participation runner's
// stop function: call it at shutdown before previews, the dispatcher, and
// the tool runtime go away, so in-flight proactive turns drain while every
// dependency they use is still alive. A nil client or persona keeps the
// feature entirely off.
func (b *Bot) SetProactiveDecisions(client interactivity.DecisionClient, persona interactivity.PersonaSource, inputBudgetBytes int64) func() {
	if client == nil || persona == nil {
		return nil
	}

	runner := newParticipationRunner(b.ctx, b.cfg.ProcessingTimeout, b.logger)
	decisions := &proactiveDecisions{
		history: botDecisionHistory{builder: b.replyCtx},
		persona: persona,
		budget:  inputBudgetBytes,
	}
	decisions.engineFor = func(resolved adminconfig.ResolvedChatConfig) interactionDecider {
		engine, err := interactivity.NewEngine(interactivity.EngineDeps{
			Client:           client,
			Models:           snapshotModels{resolved: resolved},
			Persona:          persona,
			History:          decisions.history,
			InputBudgetBytes: decisions.budget,
		}, b.logger)
		if err != nil {
			// Wiring misconfiguration: treated as feature-off for this task.
			b.logger.Error("proactive participation disabled by wiring error", "error", err)

			return nil
		}

		return engine
	}
	b.decisions = decisions
	b.participation = runner

	return runner.Close
}

// shouldEvaluateParticipation is the proactive eligibility gate applied to
// every message that did not produce an explicit turn. Private chats are
// excluded by design: private messages are inherently addressed to the bot,
// so their explicit-trigger semantics are unchanged and the engine never
// evaluates them. The disabled interactivity mode blocks the engine entirely
// (the existing admin-DM explicit exception is unaffected — it is an
// explicit turn, never an engine candidate), bot senders are never
// evaluated, and the feature must be wired.
func (b *Bot) shouldEvaluateParticipation(resolved adminconfig.ResolvedChatConfig, message t.Message) bool {
	if b.decisions == nil {
		return false
	}
	// Only the interactive modes enable proactive participation. This
	// excludes disabled outright (the admin-DM explicit exception lives in
	// the explicit gate, never here) and any unknown mode value: an
	// unrecognized configuration must not silently open the engine.
	if resolved.InteractivityMode != adminconfig.InteractivityMentionsOnly &&
		resolved.InteractivityMode != adminconfig.InteractivityMentionsReplies {
		return false
	}
	if message.From != nil && message.From.IsBot {
		return false
	}
	if b.isPrivateWithMe(message) {
		return false
	}

	return true
}

// evaluateProactiveParticipation queues the decision task for the message's
// chat. Per-chat serialization bounds concurrency to one in-flight decision
// per chat; every eligible message is evaluated, nothing is dropped, and
// the handler goroutine never blocks on the engine. Explicit turns do not
// touch the slots and are never delayed by a waiting proactive task.
func (b *Bot) evaluateProactiveParticipation(_ context.Context, message t.Message) {
	if b.participation == nil {
		return
	}

	b.participation.Submit(message.Chat.ID, func(taskCtx context.Context) {
		b.evaluateParticipation(taskCtx, message)
	})
}

// evaluateParticipation decides whether and how the bot participates
// proactively in response to one non-explicit message, then runs the
// matching proactive turn. All failures are silent by design: a failed
// decision and an ignored one are equally invisible to chat participants.
func (b *Bot) evaluateParticipation(ctx context.Context, message t.Message) {
	logger := b.loggerFromContext(ctx)

	// The operation snapshot: one settings read, taken after slot
	// acquisition. The decision and its resulting action share it — the
	// engine cannot evaluate one model pair while the action executes with
	// another.
	resolved, ok := b.resolveChatConfig(ctx, message)
	if !ok {
		logger.Warn("proactive participation skipped", "chat_id", message.Chat.ID, "reason", "chat config resolution failed")

		return
	}

	// Re-check eligibility against the operation snapshot: the pre-Submit
	// gate read the handler's resolve, and a task can sit queued behind the
	// chat slot while the mode flips to disabled. Hard access restrictions
	// stay effective at execution time; no extra settings reads are needed —
	// this consumes the snapshot above.
	if !b.shouldEvaluateParticipation(resolved, message) {
		return
	}

	engine := b.decisionEngineFor(resolved)
	if engine == nil {
		return
	}

	// The operation's persona template body, captured once at snapshot time:
	// the decision context renders from it, and the resulting action turn
	// renders from the same body across every tool-loop iteration — a
	// template or settings change mid-operation cannot split the persona
	// between the decision and the action. A capture failure fails the
	// operation silently: proceeding with a live-resolved body would risk
	// exactly the drift the capture exists to prevent.
	templateBody, err := b.decisions.persona.CapturePersonaTemplate(ctx, message.Chat.ID)
	if err != nil {
		logger.Warn("proactive participation skipped", "reason", "persona template capture failed")

		return
	}
	capturedPersona := llm.CapturedPersona{
		TemplateBody:  templateBody,
		Language:      resolved.Language,
		Gender:        resolved.Gender,
		CharacterName: resolved.CharacterName,
	}

	// Target preparation is the engine's boundary, not the bot's: it may
	// download the photo and run vision inference, so the engine invokes it
	// only after its NoOp and decision-model gates pass. Memoized once per
	// operation, the resulting action reuses the work without repeating it.
	var (
		hydrateOnce sync.Once
		target      state.Message
		hydrateErr  error
	)
	hydrate := func(taskCtx context.Context) (state.Message, error) {
		hydrateOnce.Do(func() {
			// The target renders exactly once in the decision context: the
			// engine excludes it from the history snapshot by chat and
			// message ID.
			prepared := b.tgUserMessageToMessageData(message, true)
			b.ensureMessageImageDescription(taskCtx, &prepared)
			target = prepared
		})

		return target, hydrateErr
	}

	started := time.Now()
	decision, err := engine.Decide(ctx, interactivity.DecisionInput{
		ChatID:        message.Chat.ID,
		TopicID:       message.MessageThreadID,
		TargetID:      message.MessageID,
		HydrateTarget: hydrate,
		Profile:       resolved.InteractionProfile,
		Instructions:  resolved.DecisionInstructions,
		BotUserID:     b.me.ID,
		// The persona comes from the same snapshot — settings plus the
		// captured template body: the decision and the action must describe
		// the same character even if settings change while the decision
		// request is in flight.
		Persona: interactivity.PersonaSettings{
			Language:      resolved.Language,
			Gender:        resolved.Gender,
			CharacterName: resolved.CharacterName,
			TemplateBody:  templateBody,
		},
	})
	if err != nil {
		// The engine returns typed, content-free errors only. Expected
		// outage classes stay out of Sentry; response-validation anomalies
		// are captured sanitized.
		logger.Warn("proactive decision failed",
			"chat_id", message.Chat.ID,
			"duration", time.Since(started).Round(time.Millisecond).String(),
			"error", err)
		if errors.Is(err, interactivity.ErrInvalidResponse) {
			sentry.CaptureException(err)
		}

		return
	}

	if decision.Action == interactivity.ActionIgnore {
		return
	}

	// A positive decision implies the engine prepared the target already:
	// the memoized provider returns it without repeating the work.
	target, hydrateErr = hydrate(ctx)
	if hydrateErr != nil {
		logger.Warn("proactive participation dropped", "reason", "target preparation failed")

		return
	}

	// The action receives the same resolved config and captured persona the
	// engine consumed.
	switch decision.Action {
	case interactivity.ActionReact:
		b.runProactiveReaction(ctx, message, target, resolved, &capturedPersona)
	case interactivity.ActionReply:
		b.runProactiveReply(ctx, message, target, resolved, &capturedPersona)
	case interactivity.ActionIgnore:
	}
}

// decisionEngineFor builds the engine for one operation through the wiring's
// builder. The shared boundaries (client, persona, history, budget) come
// from the wiring; the model resolver adapts the operation snapshot, so
// decision and action see the same model pair without a second settings
// read inside the engine.
func (b *Bot) decisionEngineFor(resolved adminconfig.ResolvedChatConfig) interactionDecider {
	if b.decisions == nil {
		return nil
	}

	return b.decisions.engineFor(resolved)
}

// runProactiveReaction runs the restricted reaction-only turn: previews
// off, failures silent, the only advertised and executable tool is
// set_reaction, and the run completes on the first successfully applied
// reaction. Any produced text is discarded — never published, never
// recorded. Album targets are refused by the tool itself via the media
// group ID the pipeline forwards. persona is the operation's captured
// persona: every generation iteration of the turn renders it.
func (b *Bot) runProactiveReaction(ctx context.Context, message t.Message, userMessageData state.Message, resolved adminconfig.ResolvedChatConfig, persona *llm.CapturedPersona) {
	b.runConversationTurn(ctx, message, userMessageData, resolved, turnOptions{
		trigger:                 llmcontext.TriggerProactive,
		errorPolicy:             silentErrors,
		delivery:                discardText,
		withPreview:             false,
		allowedTools:            []string{"set_reaction"},
		completeAfterSideEffect: true,
		persona:                 persona,
	})
}

// runProactiveReply runs the ordinary turn with proactive policies: previews
// on, the full tool set, text delivered and recorded, and failures silent —
// an unsolicited answer never surfaces error notices or fallbacks. persona
// is the operation's captured persona: every generation iteration of the
// turn renders it.
func (b *Bot) runProactiveReply(ctx context.Context, message t.Message, userMessageData state.Message, resolved adminconfig.ResolvedChatConfig, persona *llm.CapturedPersona) {
	b.runConversationTurn(ctx, message, userMessageData, resolved, turnOptions{
		trigger:     llmcontext.TriggerProactive,
		errorPolicy: silentErrors,
		delivery:    deliverAndRecord,
		withPreview: true,
		persona:     persona,
	})
}

// snapshotModels adapts the operation-level settings snapshot to the
// engine's model resolver. Unset (empty) and failed are distinct by
// construction: the snapshot was taken once, and an empty field means the
// unset override chain (environment route or unconfigured feature), never
// a resolution error.
type snapshotModels struct {
	resolved adminconfig.ResolvedChatConfig
}

func (s snapshotModels) GenerationModel(_ context.Context, _ int64) (string, error) {
	return s.resolved.ChatModel, nil
}

func (s snapshotModels) DecisionModel(_ context.Context, _ int64) (string, error) {
	return s.resolved.SystemOneModel, nil
}

// botDecisionHistory adapts the reply-context builder to the engine's
// history source: the same bounding and hydration as the ordinary turn
// path, without a system hint or a current-message slot. The builder cannot
// fail, so the error return only satisfies the interface.
type botDecisionHistory struct {
	builder *llmcontext.ReplyBuilder
}

func (h botDecisionHistory) DecisionHistory(ctx context.Context, scope state.ConversationScope) (interactivity.HistorySnapshot, error) {
	messages, earlierSummary := h.builder.DecisionHistory(ctx, scope)

	return interactivity.HistorySnapshot{Messages: messages, EarlierSummary: earlierSummary}, nil
}
