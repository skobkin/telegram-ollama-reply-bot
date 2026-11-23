package bot

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"telegram-ollama-reply-bot/internal/adminconfig"
	"telegram-ollama-reply-bot/internal/execution"
	"telegram-ollama-reply-bot/internal/llm"
	"telegram-ollama-reply-bot/internal/llmcontext"
	"telegram-ollama-reply-bot/internal/state"
	"telegram-ollama-reply-bot/internal/support/errreport"
	"telegram-ollama-reply-bot/internal/tooluse"

	t "github.com/mymmrac/telego"
	tu "github.com/mymmrac/telego/telegoutil"
)

// turnDeliveryPolicy selects what happens to a produced text reply.
type turnDeliveryPolicy uint8

const (
	// deliverAndRecord sends the final text and records it in history —
	// the explicit and proactive-reply behavior.
	deliverAndRecord turnDeliveryPolicy = iota
	// discardText suppresses the produced text: it is never published and
	// never recorded (the restricted reaction-only turn).
	discardText
)

// turnErrorPolicy selects how a failed turn surfaces to chat participants.
type turnErrorPolicy uint8

const (
	// explicitErrors keeps the existing user-facing fallbacks: timeout and
	// generic error notices, and a reply-failure notice.
	explicitErrors turnErrorPolicy = iota
	// silentErrors presents nothing user-visible: content-free diagnostics
	// to logs and Sentry plus preview cleanup only (proactive turns).
	silentErrors
)

// turnOptions select the behavior of one shared pipeline invocation. They
// separate the explicit-turn behavior (unchanged) from the proactive turn
// policies decided at planning: previews on for replies and off for
// reactions, silent failures for everything proactive, text delivered and
// recorded only for replies.
type turnOptions struct {
	trigger llmcontext.TriggerKind
	// errorPolicy picks the user-facing error presentation.
	errorPolicy turnErrorPolicy
	// delivery picks the text outcome on success.
	delivery turnDeliveryPolicy
	// withPreview registers a live preview run for this turn; reactions run
	// without one (no RunFor, no typing fallback beyond the deadline
	// wrapper's default).
	withPreview bool
	// allowedTools restricts the turn's tool advertisement and execution;
	// nil keeps the full registry (explicit and proactive replies).
	allowedTools []string
	// completeAfterSideEffect ends a restricted run on the first
	// successfully applied side effect (reaction-only turns).
	completeAfterSideEffect bool
	// typingFallback lets the no-preview typing indicator run. Only the
	// explicit turn sets it: unsolicited proactive participation must not
	// announce itself with a typing indicator, with or without previews.
	typingFallback bool
	// persona carries the proactive operation's captured persona into the
	// generation request: every tool-loop iteration renders the system
	// prompt from it instead of resolving settings and the template body
	// live, so the turn keeps describing the character its operation
	// started with. Nil keeps the ordinary live rendering (explicit turns).
	persona *llm.CapturedPersona
}

// turnFailure is the bounded failure classification of one conversation
// turn, computed inside the pipeline where the error is fresh.
type turnFailure uint8

const (
	turnFailureNone turnFailure = iota
	turnFailureTimeout
	turnFailureCancelled
	turnFailureGeneration
	turnFailureDelivery
)

// turnEffects is the statistics-facing summary of one conversation turn.
// The per-call classifications are mutually exclusive at the tool level
// (each successful call contributes to exactly one of reaction confirmed /
// other side effect / neither), while the turn-level flags may co-occur: a
// reply turn with a successful reaction and another side-effecting tool
// sets both. Effect flags are counted regardless of completion — a partial
// success (reaction applied, generation failed) reports both the flag and
// the failure.
type turnEffects struct {
	ReactionConfirmed         bool
	TextConfirmed             bool
	OtherSideEffectsConfirmed bool
	Failure                   turnFailure
}

// turnFailureFromError classifies a turn's terminal error. The pipeline's
// processing deadline surfaces as ErrRequestTimeout (not the bare
// context sentinel) on two of its three branches, so that identity leads;
// in-flight backend aborts keep context.Canceled in their chain.
func turnFailureFromError(err error) turnFailure {
	switch {
	case err == nil:
		return turnFailureNone
	case errors.Is(err, ErrRequestTimeout):
		return turnFailureTimeout
	case errors.Is(err, context.Canceled):
		return turnFailureCancelled
	default:
		return turnFailureGeneration
	}
}

// turnEffectsFromToolCalls derives the effect flags from the run's
// authoritative tool-call records. set_reaction success is the reaction
// fact; any OTHER successful side-effecting definition is the other-side
// effect fact (read-only tools also return OK and are excluded by the
// SideEffecting metadata).
func turnEffectsFromToolCalls(records []tooluse.ToolCallRecord) (reaction, other bool) {
	for _, record := range records {
		if record.Status != execution.StatusOK {
			continue
		}
		if record.Name == "set_reaction" {
			reaction = true

			continue
		}
		if record.SideEffecting {
			other = true
		}
	}

	return reaction, other
}

// effectsFromCalls builds the summary from the run's tool records plus the
// text-delivery fact.
func effectsFromCalls(records []tooluse.ToolCallRecord, textConfirmed bool) turnEffects {
	effects := turnEffects{TextConfirmed: textConfirmed}
	effects.ReactionConfirmed, effects.OtherSideEffectsConfirmed = turnEffectsFromToolCalls(records)

	return effects
}

// runConversationTurn is the shared generation-turn pipeline: preview
// registration, history summarization, reply-context construction, the
// tool loop, error presentation, delivery, and history recording. Explicit
// and proactive turns share it; the options carry the per-kind policies.
// The caller supplies the base context (the handler context for explicit
// turns, the task deadline context for proactive ones) and the message data
// built once for the operation. It returns the turn's effect summary for
// statistics; explicit callers may ignore it.
func (b *Bot) runConversationTurn(ctx context.Context, message t.Message, userMessageData state.Message, resolved adminconfig.ResolvedChatConfig, opts turnOptions) turnEffects {
	chatID := tu.ID(message.Chat.ID)

	// The preview starts before preprocessing so the neutral indicator
	// covers the entire run, history summarization included. It returns nil
	// (and everything below no-ops) when previews are off or the turn runs
	// without one.
	var run *previewRun
	if opts.withPreview {
		run = b.previews.RunFor(RunInfo{
			RunID:            execution.NewRunID(),
			ChatID:           message.Chat.ID,
			TopicID:          message.MessageThreadID,
			TriggerMessageID: message.MessageID,
			ChatType:         message.Chat.Type,
			Mode:             resolved.PreviewMode,
			KeepToolHistory:  resolved.KeepToolHistory,
		})
		defer run.Close() // safety net: state cleanup on every exit path
	}

	b.maybeSummarizeHistory(ctx, message, opts.errorPolicy == silentErrors)
	logger := b.loggerFromContext(ctx)
	logger.Debug("handling mention")

	var llmReply string
	var usage *llm.TokenUsage
	var toolCalls []tooluse.ToolCallRecord
	var advertisedTools []string
	var intentionalSilence bool
	var err error

	// The tool statistics epoch is captured BEFORE the run: a reset while the
	// turn is in flight drops the whole turn's tool observation.
	toolEpoch := b.stats.ToolEpoch()

	turnWork := func(ctx context.Context) error {
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
			Trigger:        opts.trigger,
			// Only the restricted reaction turn completes on a side effect;
			// its model must know text is pointless before it writes any.
			ReactionOnly: opts.completeAfterSideEffect,
		})

		// A captured persona freezes the character for the whole proactive
		// operation, across every tool-loop iteration; nil keeps the
		// ordinary live rendering (explicit turns).
		requestContext.Persona = opts.persona

		llmCtx, cancel := b.withProcessingDeadline(ctx)
		defer cancel()

		result, runErr := b.tools.ReplyWithTools(llmCtx, tooluse.ChatRequest{
			ChatTitle:      message.Chat.Title,
			Scope:          scopeFromMessage(message),
			PromptScope:    llm.PromptScope{ChatID: message.Chat.ID},
			ReplyContext:   requestContext,
			RequestMessage: userMessageData,
			RunID:          run.ID(),
			// The model selection comes from the chat config resolved when
			// this message was accepted; it stays fixed for the whole turn.
			TurnConfig: llm.TurnConfig{
				GenerationModel:        resolved.ChatModel,
				SystemOneModel:         resolved.SystemOneModel,
				WebSearchEnabled:       resolved.WebSearchEnabled,
				ImageGenerationEnabled: resolved.ImageGenerationEnabled,
				ImageGenerationModel:   resolved.ImageGenerationModel,
			},
			RequesterMediaGroupID:   message.MediaGroupID,
			AllowedTools:            opts.allowedTools,
			CompleteAfterSideEffect: opts.completeAfterSideEffect,
			// Silent turns sanitize generation diagnostics at every layer:
			// provider error bodies can echo chat content, and nothing
			// user-initiated is waiting on the details.
			SilentErrors: opts.errorPolicy == silentErrors,
			// The trigger is derived from Telegram metadata (mention, reply,
			// private chat vs the participation engine), never from the model.
			Proactive: opts.trigger == llmcontext.TriggerProactive,
		})
		llmReply, usage, err = result.Reply, result.Usage, runErr
		toolCalls = result.ToolCalls
		advertisedTools = result.AdvertisedTools
		intentionalSilence = result.IntentionalSilence

		return runErr
	}

	// The typing fallback covers turns without an active preview run
	// (explicit turns, and replies when previews are off). Proactive turns
	// suppress it entirely: a silent reaction must not announce itself as
	// an incoming reply.
	if opts.typingFallback {
		err = b.runWithPreviewTimeout(ctx, chatID, run, turnWork, func() bool { return intentionalSilence })
	} else {
		deadlineCtx, cancel := b.withProcessingDeadline(ctx)
		defer cancel()
		err = b.withinProcessingDeadline(deadlineCtx, turnWork, func() bool { return intentionalSilence })
	}

	// One authoritative tool-statistics observation per turn on every exit
	// path (success, silence, failure, cancellation, timeout), recorded after
	// the loop and before any final text delivery.
	b.recordToolTurn(toolEpoch, message.Chat.ID, opts.trigger, b.effectiveGenerationModel(resolved), advertisedTools, toolCalls, err)

	if err != nil {
		effects := effectsFromCalls(toolCalls, false)
		effects.Failure = turnFailureFromError(err)

		if opts.errorPolicy == silentErrors {
			// Proactive turns fail silent: no user-facing notices, no
			// fallback provider — content-free diagnostics only. The failed
			// turn's preview message is deleted: an unsolicited working
			// indicator must not outlive an unsolicited turn.
			run.Fail()
			logger.Error("proactive turn failed", "error", err)
			// A timeout is already counted in the LLM timeout statistics and
			// is not reported as a defect, as on the explicit path below.
			if !errors.Is(err, ErrRequestTimeout) {
				errreport.Capture(err)
			}

			return effects
		}

		run.Discard()

		if errors.Is(err, ErrRequestTimeout) {
			logger.Error("llm request timed out", "chat_id", message.Chat.ID, "error", err)
			timeout := b.cfg.ProcessingTimeout
			_, _ = b.sendMessage(ctx, b.reply(message, tu.Message(
				chatID,
				fmt.Sprintf("LLM request timed out after %s. Try again later.", timeout),
			)))

			return effects
		}

		logger.Error("cannot get reply from llm connector", "error", err)
		errreport.Capture(err)

		_, _ = b.sendMessage(ctx, b.reply(message, tu.Message(
			chatID,
			"LLM request error. Try again later.",
		)))

		return effects
	}

	if usage != nil {
		b.stats.AddUsage(usage.PromptTokens, usage.CompletionTokens, usage.TotalTokens, usage.Cost)
	}
	if intentionalSilence {
		run.Discard()

		return effectsFromCalls(toolCalls, false)
	}

	logger.Debug("sending llm reply", "reply_length", len(llmReply))

	if strings.TrimSpace(llmReply) == "" {
		logger.Debug("llm interaction completed without final text reply")
		run.CompleteNoText(toolCalls)

		return effectsFromCalls(toolCalls, false)
	}

	if opts.delivery == discardText {
		// The suppressed text is never published and never recorded in
		// history: the reaction-only outcome is the applied side effect, or
		// nothing.

		return effectsFromCalls(toolCalls, false)
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
	sent, err := b.deliverFinalReply(ctx, message, llmReply, toolCalls, resolved)
	if err != nil {
		if opts.errorPolicy == silentErrors {
			run.Fail()
		} else {
			run.Discard()
		}

		logger.Error("cannot send reply message", "error", err, "reply_length", len(llmReply))
		errreport.Capture(err)

		if opts.errorPolicy == explicitErrors {
			b.trySendReplyError(ctx, message)
		}

		effects := effectsFromCalls(toolCalls, false)
		effects.Failure = turnFailureDelivery

		return effects
	}

	run.FinalDelivered()

	replyMessageID := 0
	if sent != nil {
		replyMessageID = sent.MessageID
	}

	b.saveBotReplyToHistory(ctx, message, llmReply, replyMessageID)

	return effectsFromCalls(toolCalls, true)
}
