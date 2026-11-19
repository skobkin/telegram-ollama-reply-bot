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
	"telegram-ollama-reply-bot/internal/tooluse"

	"github.com/getsentry/sentry-go"
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

// runConversationTurn is the shared generation-turn pipeline: preview
// registration, history summarization, reply-context construction, the
// tool loop, error presentation, delivery, and history recording. Explicit
// and proactive turns share it; the options carry the per-kind policies.
// The caller supplies the base context (the handler context for explicit
// turns, the task deadline context for proactive ones) and the message data
// built once for the operation.
func (b *Bot) runConversationTurn(ctx context.Context, message t.Message, userMessageData state.Message, resolved adminconfig.ResolvedChatConfig, opts turnOptions) {
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
	var err error

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
			Scope:          scopeFromMessage(message),
			PromptScope:    llm.PromptScope{ChatID: message.Chat.ID},
			ReplyContext:   requestContext,
			RequestMessage: userMessageData,
			RunID:          run.ID(),
			// The model selection comes from the chat config resolved when
			// this message was accepted; it stays fixed for the whole turn.
			TurnConfig: llm.TurnConfig{
				GenerationModel: resolved.ChatModel,
				SystemOneModel:  resolved.SystemOneModel,
			},
			RequesterMediaGroupID:   message.MediaGroupID,
			AllowedTools:            opts.allowedTools,
			CompleteAfterSideEffect: opts.completeAfterSideEffect,
			// Silent turns sanitize generation diagnostics at every layer:
			// provider error bodies can echo chat content, and nothing
			// user-initiated is waiting on the details.
			SilentErrors: opts.errorPolicy == silentErrors,
		})
		llmReply, usage, err = result.Reply, result.Usage, runErr
		toolCalls = result.ToolCalls

		return runErr
	}

	// The typing fallback covers turns without an active preview run
	// (explicit turns, and replies when previews are off). Proactive turns
	// suppress it entirely: a silent reaction must not announce itself as
	// an incoming reply.
	if opts.typingFallback {
		err = b.runWithPreviewTimeout(ctx, chatID, run, turnWork)
	} else {
		deadlineCtx, cancel := b.withProcessingDeadline(ctx)
		defer cancel()
		err = b.withinProcessingDeadline(deadlineCtx, turnWork)
	}
	if err != nil {
		if opts.errorPolicy == silentErrors {
			// Proactive turns fail silent: no user-facing notices, no
			// fallback provider — content-free diagnostics only. The failed
			// turn's preview message is deleted: an unsolicited working
			// indicator must not outlive an unsolicited turn.
			run.Fail()
			logger.Error("proactive turn failed", "error", err)
			sentry.CaptureException(err)

			return
		}

		run.Abort()

		if errors.Is(err, ErrRequestTimeout) {
			logger.Error("llm request timed out", "chat_id", message.Chat.ID, "error", err)
			timeout := b.cfg.ProcessingTimeout
			_, _ = b.sendMessage(ctx, b.reply(message, tu.Message(
				chatID,
				fmt.Sprintf("LLM request timed out after %s. Try again later.", timeout),
			)))

			return
		}

		logger.Error("cannot get reply from llm connector", "error", err)
		sentry.CaptureException(err)

		_, _ = b.sendMessage(ctx, b.reply(message, tu.Message(
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

	if opts.delivery == discardText {
		// The suppressed text is never published and never recorded in
		// history: the reaction-only outcome is the applied side effect, or
		// nothing.
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
	sent, err := b.deliverFinalReply(ctx, message, llmReply, toolCalls, resolved)
	if err != nil {
		if opts.errorPolicy == silentErrors {
			run.Fail()
		} else {
			run.Abort()
		}

		logger.Error("cannot send reply message", "error", err, "reply_length", len(llmReply))
		sentry.CaptureException(err)

		if opts.errorPolicy == explicitErrors {
			b.trySendReplyError(ctx, message)
		}

		return
	}

	run.FinalDelivered()

	replyMessageID := 0
	if sent != nil {
		replyMessageID = sent.MessageID
	}

	b.saveBotReplyToHistory(ctx, message, llmReply, replyMessageID)
}
