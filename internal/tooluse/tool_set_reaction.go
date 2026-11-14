package tooluse

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	"telegram-ollama-reply-bot/internal/execution"
	"telegram-ollama-reply-bot/internal/state"
	"telegram-ollama-reply-bot/internal/telegram/reactionemoji"

	t "github.com/mymmrac/telego"
	telegoapi "github.com/mymmrac/telego/telegoapi"
	tu "github.com/mymmrac/telego/telegoutil"
)

// Reaction coordination limits. The durations live on reactionLimiter so
// tests can shrink them without sleeping through real ones.
const (
	// reactionChatSpacing is the baseline per-chat spacing between outgoing
	// reaction sends, reserved by every proceeding call.
	reactionChatSpacing = 1 * time.Second
	// reactionRateLimitPad extends a Telegram 429 retry_after by a safety
	// margin for clock skew between this server and Telegram.
	reactionRateLimitPad = 1 * time.Second
	// reactionMaxWait bounds how long one call may wait on a chat cooldown
	// or a busy target gate before refusing. It never shortens a stored
	// cooldown itself — later callers still refuse while the deadline from
	// Telegram stands.
	reactionMaxWait = 30 * time.Second
	// Soft caps for the coordination maps. Both may be exceeded while all
	// their entries are active (held or waited-on gates, unexpired
	// cooldowns): dropping active entries would break serialization or let
	// sends into a known rate-limited window. Free gates carry no state, so
	// evicting them loses nothing — the target simply gets a fresh gate on
	// its next use; no separate idle TTL is needed.
	reactionGateMaxEntries     = 1024
	reactionCooldownMaxEntries = 4096
)

// errReactionRateLimited marks a call refused because the chat is inside a
// Telegram rate-limit window.
var errReactionRateLimited = errors.New("reaction rate limited")

// errReactionTargetBusy marks a call that gave up waiting for its target
// gate: another run's send to the same message held it for too long.
var errReactionTargetBusy = errors.New("reaction target busy")

// reactionTargetKey identifies the Telegram message a reaction send
// serializes on. Telegram allows one bot reaction per message and the
// remote apply order of concurrent sends is undefined, so same-target sends
// must not overlap.
type reactionTargetKey struct {
	chatID    int64
	messageID int
}

// reactionGate is a capacity-1 semaphore serializing sends to one target
// message: a token in the channel means a send is in flight. refs counts
// registered users (holders and waiters) so eviction can never drop a gate
// someone is waiting on. The channel keeps context cancellation leak-free:
// exactly one of "slot acquired" and "context done" ever wins, so a
// cancelled waiter leaves neither a phantom holder nor an orphaned
// registration behind.
type reactionGate struct {
	slots chan struct{}
	refs  int
}

// reactionLimiter coordinates outgoing set-reaction API calls across
// concurrent runs: per-chat pacing (baseline spacing plus Telegram 429
// deadlines) and per-target serialization. Its mutex guards map access
// only — never a wait and never a network call. Zero value ready to use.
type reactionLimiter struct {
	mu sync.Mutex

	// Tuneables for tests; zero falls back to the constants above.
	spacing time.Duration
	pad     time.Duration
	maxWait time.Duration

	cooldowns map[int64]time.Time
	gates     map[reactionTargetKey]*reactionGate
}

func (l *reactionLimiter) spacingDuration() time.Duration {
	if l.spacing > 0 {
		return l.spacing
	}

	return reactionChatSpacing
}

func (l *reactionLimiter) padDuration() time.Duration {
	if l.pad > 0 {
		return l.pad
	}

	return reactionRateLimitPad
}

func (l *reactionLimiter) maxWaitDuration() time.Duration {
	if l.maxWait > 0 {
		return l.maxWait
	}

	return reactionMaxWait
}

// acquire serializes on the target message and returns the gate the caller
// must release after its send attempt (success or failure). Waiting is
// bounded by the per-call wait bound and the caller's context, whichever
// comes first — a stalled holder must never stall other runs indefinitely.
func (l *reactionLimiter) acquire(ctx context.Context, chatID int64, messageID int) (*reactionGate, error) {
	key := reactionTargetKey{chatID: chatID, messageID: messageID}

	l.mu.Lock()
	if l.gates == nil {
		l.gates = make(map[reactionTargetKey]*reactionGate)
	}
	l.pruneGatesLocked()
	gate, ok := l.gates[key]
	if !ok {
		gate = &reactionGate{slots: make(chan struct{}, 1)}
		l.gates[key] = gate
	}
	gate.refs++
	l.mu.Unlock()

	timer := time.NewTimer(l.maxWaitDuration())
	defer timer.Stop()

	select {
	case gate.slots <- struct{}{}:
		return gate, nil
	case <-ctx.Done():
		l.unregister(key, gate)

		return nil, ctx.Err()
	case <-timer.C:
		l.unregister(key, gate)

		return nil, errReactionTargetBusy
	}
}

// release frees a gate acquired by acquire. The caller holds the token, so
// the handoff receive cannot block.
func (l *reactionLimiter) release(chatID int64, messageID int, gate *reactionGate) {
	<-gate.slots
	l.unregister(reactionTargetKey{chatID: chatID, messageID: messageID}, gate)
}

// unregister drops one registration. Called after the token handoff in both
// the success and the cancelled path.
func (l *reactionLimiter) unregister(_ reactionTargetKey, gate *reactionGate) {
	l.mu.Lock()
	gate.refs--
	l.pruneGatesLocked()
	l.mu.Unlock()
}

// pruneGatesLocked drops unregistered (free) gates when the map exceeds its
// soft cap. Gates with refs > 0 — held sends and registered waiters — are
// never dropped: dropping one would let two runs send to the same message
// at once.
func (l *reactionLimiter) pruneGatesLocked() {
	if len(l.gates) <= reactionGateMaxEntries {
		return
	}

	for key, gate := range l.gates {
		if gate.refs <= 0 {
			delete(l.gates, key)
		}
	}
}

// awaitChat waits for the chat's reaction cooldown to pass and reserves the
// next send slot atomically in the same locked step, so concurrent callers
// space their sends even when they waited together. It refuses instead of
// waiting when the stored deadline exceeds the per-call wait bound, and it
// never waits longer than the caller's context allows.
func (l *reactionLimiter) awaitChat(ctx context.Context, chatID int64) error {
	refuseAt := time.Now().Add(l.maxWaitDuration())

	for {
		l.mu.Lock()
		now := time.Now()
		if next, tracked := l.cooldowns[chatID]; tracked && next.After(now) {
			l.mu.Unlock()

			if next.After(refuseAt) {
				return errReactionRateLimited
			}

			timer := time.NewTimer(time.Until(next))
			select {
			case <-ctx.Done():
				timer.Stop()

				return ctx.Err()
			case <-timer.C:
			}

			continue
		}

		// The slot is free: reserve it for this chat under the same lock
		// step that verified it, so no other caller can squeeze in between.
		// Ordinary reservations prune too — without a 429 nothing else
		// would ever collect expired entries, and one chat per entry the
		// map would grow for the process lifetime.
		if l.cooldowns == nil {
			l.cooldowns = make(map[int64]time.Time)
		}
		l.cooldowns[chatID] = now.Add(l.spacingDuration())
		l.pruneCooldownsLocked()
		l.mu.Unlock()

		return nil
	}
}

// penalizeChat records a Telegram 429: the full retry deadline — never
// shortened — becomes the chat's earliest reaction-send time, overriding the
// baseline spacing reservation.
func (l *reactionLimiter) penalizeChat(chatID int64, retryAfter time.Duration) {
	until := time.Now().Add(retryAfter + l.padDuration())

	l.mu.Lock()
	if l.cooldowns == nil {
		l.cooldowns = make(map[int64]time.Time)
	}
	if until.After(l.cooldowns[chatID]) {
		l.cooldowns[chatID] = until
	}
	l.pruneCooldownsLocked()
	l.mu.Unlock()
}

// pruneCooldownsLocked drops expired cooldown entries when the map exceeds
// its soft cap. Unexpired deadlines — including Telegram-imposed windows no
// caller may send into — are never dropped.
func (l *reactionLimiter) pruneCooldownsLocked() {
	if len(l.cooldowns) <= reactionCooldownMaxEntries {
		return
	}

	now := time.Now()
	for chatID, deadline := range l.cooldowns {
		if !deadline.After(now) {
			delete(l.cooldowns, chatID)
		}
	}
}

// setReactionDescription builds the model-visible tool description from one
// canonical source: the guidance is spec-mandated (it must reach the model
// even when custom prompt templates drop the tool-policy section, because
// tool definitions always travel with the request) and the emoji list is
// generated from the shared whitelist.
func setReactionDescription() string {
	var sb strings.Builder
	sb.WriteString("React to the message that triggered this run with exactly one supported emoji. ")
	sb.WriteString("The target is the user message that started the current run — never assume it is the latest message in the chat. ")
	sb.WriteString("If a reaction fully communicates the response, you may end the turn without any additional text; you may also react and then provide a text reply when useful. ")
	sb.WriteString("Do not replace a substantive answer to a genuine question or request with a reaction alone, and do not repeat or change reactions unnecessarily. ")
	sb.WriteString("Available emoji: " + reactionemoji.CandidatesJoined() + ". ")
	sb.WriteString("These are the bot's supported candidates, not a guarantee that Telegram accepts them in every chat.")

	return sb.String()
}

// setReactionParameters builds the JSON Schema from the shared whitelist's
// reaction candidates so the enum can never drift from the validated set.
func setReactionParameters() json.RawMessage {
	// json.Marshal on a []string cannot fail.
	enum, _ := json.Marshal(reactionemoji.ReactionCandidates())

	return json.RawMessage(`{"type":"object","properties":{"emoji":{"type":"string","enum":` + string(enum) + `,"description":"Exactly one supported reaction emoji"}},"required":["emoji"],"additionalProperties":false}`)
}

// setReaction reacts to the message that triggered the current run with one
// supported emoji, alone or alongside a later text reply. See
// docs/specs/model_authored_reactions.md for the normative behavior.
func (r *Runtime) setReaction(ctx context.Context, callCtx CallContext, args json.RawMessage) (toolResult, error) {
	var payload struct {
		Emoji string `json:"emoji"`
	}
	if !decodeToolJSON(args, &payload) {
		return setReactionError("invalid_arguments", `Provide the reaction as a JSON object with one "emoji" field, e.g. {"emoji":"👍"}.`), nil
	}

	if !reactionemoji.Candidate(payload.Emoji) {
		return setReactionError("invalid_emoji", "Unsupported emoji: choose exactly one emoji from the listed available values."), nil
	}

	// The target is bound exclusively to this invocation's immutable request
	// context. CallContext.Requester.MessageID is the Telegram message that
	// triggered THIS run — it is not "the latest message in the chat": it
	// does not change when other users send messages or when multiple runs
	// execute concurrently, and it must never be derived from history or any
	// global "current message" variable. Reacting to earlier messages or to
	// the bot's previous reply is out of scope.
	//
	// No message ID is exposed to the model in any argument, prompt, or
	// result: the binding is implicit in the run.
	target, ok := setReactionTarget(callCtx)
	if !ok || target.messageID <= 0 || target.chatID == 0 || callCtx.Requester.IsMe {
		return setReactionError("invalid_target", "No valid triggering user message is bound to this run."), nil
	}
	if callCtx.RequesterMediaGroupID != "" {
		// Telegram redirects album reactions to the first non-deleted
		// member, so the effective target cannot be determined reliably and
		// the reaction must not be falsely attributed.
		return setReactionError("media_group_target", "The triggering message is part of a media group; reactions to albums are not supported."), nil
	}

	if callCtx.run == nil {
		return setReactionError("run_state_unavailable", "Reaction state is unavailable for this call."), nil
	}
	if r.config.BotIdentity.zero() {
		return setReactionError("identity_unavailable", "The bot identity is not initialized; reactions cannot be recorded."), nil
	}

	// Cheap pre-check: a spent run is rejected before it can wait for a
	// send slot it can never use. The authoritative atomic claim happens
	// after the waits; the pre-check cannot race anything because calls of
	// one run are sequential.
	if callCtx.run.reactionAttempted.Load() {
		return setReactionError("already_reacted", "This run already attempted its one allowed reaction."), nil
	}

	// Serialize per target before pacing: two runs reacting to the same
	// message must send one after another, and each reserves its chat slot
	// only once it is the gate holder, immediately before its send. The
	// wait is bounded: a stalled holder must not stall this run's remaining
	// tools and response indefinitely.
	gate, err := r.reactions.acquire(ctx, target.chatID, target.messageID)
	if err != nil {
		if errors.Is(err, errReactionTargetBusy) {
			return setReactionError("target_busy", "Another reaction to this message is still being sent; try again later."), nil
		}

		return setReactionCancelledResult(), nil
	}
	defer r.reactions.release(target.chatID, target.messageID, gate)

	if err := r.reactions.awaitChat(ctx, target.chatID); err != nil {
		if errors.Is(err, errReactionRateLimited) {
			return setReactionError("rate_limited", "The chat is inside a Telegram rate-limit window for reactions; try again later."), nil
		}

		return setReactionCancelledResult(), nil
	}

	// All blocking waits are done. Claim the run's single outgoing attempt
	// only now, so a cancelled or refused wait never consumes it. Tool calls
	// within a run are sequential, so the claim has no in-run race. Invalid
	// local arguments above never reach this claim.
	if ctx.Err() != nil {
		return setReactionCancelledResult(), nil
	}
	if !callCtx.run.reactionAttempted.CompareAndSwap(false, true) {
		return setReactionError("already_reacted", "This run already attempted its one allowed reaction."), nil
	}

	params := &t.SetMessageReactionParams{
		ChatID:    tu.ID(target.chatID),
		MessageID: target.messageID,
		Reaction:  []t.ReactionType{&t.ReactionTypeEmoji{Type: t.ReactionEmoji, Emoji: payload.Emoji}},
		IsBig:     false,
	}

	sendErr := r.actions.SetMessageReaction(ctx, params)
	if sendErr != nil {
		return r.setReactionSendError(callCtx, target.chatID, sendErr), nil
	}

	// Telegram confirmed the reaction: record it synchronously on the
	// triggering message — the authoritative local state — before the
	// observational event. A vanished target (restart, trim, reset) stays a
	// successful tool action; the message is never resurrected.
	stored := r.history.SetBotReaction(callCtx.Scope, target.messageID, state.Reaction{
		UserID:   r.config.BotIdentity.UserID,
		Name:     r.config.BotIdentity.Name,
		Username: r.config.BotIdentity.Username,
		Emoji:    payload.Emoji,
	})
	if !stored {
		callCtx.Logger.Warn("confirmed reaction target missing from history",
			"chat_id", target.chatID, "message_id", target.messageID)
	}

	if r.config.Dispatcher != nil {
		r.config.Dispatcher.Publish(execution.BotReactionSet{
			RunContext: execution.RunContext{
				RunID:     callCtx.RunID,
				ChatID:    callCtx.Scope.ChatID,
				TopicID:   callCtx.Scope.TopicID,
				MessageID: target.messageID,
			},
			At:              time.Now(),
			TargetMessageID: target.messageID,
			BotUserID:       r.config.BotIdentity.UserID,
			Emoji:           payload.Emoji,
		})
	}

	return toolResult{
		Status:  execution.StatusOK,
		Summary: "Reacted with " + payload.Emoji,
		Data: map[string]any{
			"emoji": payload.Emoji,
		},
	}, nil
}

// setReactionSendError classifies one Telegram failure into a safe tool
// error. Descriptions coming from the Telegram API itself are safe to show;
// transport errors can embed the token-bearing request URL, so they are
// classified instead of printed — in model-facing results and in logs alike.
func (r *Runtime) setReactionSendError(callCtx CallContext, chatID int64, sendErr error) toolResult {
	var apiErr *telegoapi.Error
	switch {
	case errors.Is(sendErr, context.Canceled) || errors.Is(sendErr, context.DeadlineExceeded):
		callCtx.Logger.Warn("set_reaction cancelled during telegram request")

		return setReactionCancelledResult()
	case errors.As(sendErr, &apiErr):
		if apiErr.ErrorCode == 429 {
			var retryAfterSeconds int
			if apiErr.Parameters != nil && apiErr.Parameters.RetryAfter > 0 {
				retryAfterSeconds = apiErr.Parameters.RetryAfter
			}
			r.reactions.penalizeChat(chatID, time.Duration(retryAfterSeconds)*time.Second)
			callCtx.Logger.Warn("set_reaction rate limited by telegram", "retry_after_seconds", retryAfterSeconds)

			return setReactionError("rate_limited", "Telegram rate-limited the reaction; the chat is cooling down.")
		}

		callCtx.Logger.Warn("set_reaction rejected by telegram", "error_code", apiErr.ErrorCode)

		return setReactionError("telegram_error", "Telegram rejected the reaction: "+apiErr.Description)
	default:
		callCtx.Logger.Warn("set_reaction telegram request failed")

		return setReactionError("request_failed", "The Telegram request failed; the reaction was not applied.")
	}
}

// setReactionTarget resolves the reaction target from the invocation's
// request context and validates that the requester metadata agrees with the
// run scope.
func setReactionTarget(callCtx CallContext) (reactionTargetKey, bool) {
	if callCtx.Requester.ChatID != 0 && callCtx.Requester.ChatID != callCtx.Scope.ChatID {
		return reactionTargetKey{}, false
	}
	if callCtx.Requester.TopicID != 0 && callCtx.Scope.TopicID != 0 && callCtx.Requester.TopicID != callCtx.Scope.TopicID {
		return reactionTargetKey{}, false
	}

	return reactionTargetKey{chatID: callCtx.Scope.ChatID, messageID: callCtx.Requester.MessageID}, true
}

// setReactionError builds a structured tool error the model can read and
// react to; it is not a Go error, so the runtime treats it as a completed
// (failed) tool call.
func setReactionError(code, message string) toolResult {
	return toolResult{
		Status: execution.StatusError,
		Data: map[string]any{
			"error": map[string]any{
				"code":    code,
				"message": message,
			},
		},
	}
}

func setReactionCancelledResult() toolResult {
	return setReactionError("cancelled", "The reaction was cancelled before it could be applied.")
}
