package tooluse

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"telegram-ollama-reply-bot/internal/stickers"

	tu "github.com/mymmrac/telego/telegoutil"
)

const (
	sendStickerToolName = "send_sticker"

	// stickerProactiveCooldown is the per-chat spacing between a sticker
	// send and the next PROACTIVE one. Explicit requests (mention, reply to
	// the bot, private chat) are never declined by it.
	stickerProactiveCooldown = 30 * time.Second

	// stickerMissLimit bounds unknown or malformed sticker ids per turn;
	// they never consume the send attempt, but guessing is not unlimited.
	stickerMissLimit = 3

	// stickerCooldownMaxEntries soft-caps the cooldown map.
	stickerCooldownMaxEntries = 4096
)

// stickerLimiter spaces proactive sticker sends per chat. It is deliberately
// not a general rate limiter: one map of last-dispatch times, one mutex.
// Admission is an atomic check-and-reserve, so concurrent turns in one chat
// cannot both pass.
type stickerLimiter struct {
	mu   sync.Mutex
	last map[int64]time.Time
}

// admit checks and, when admitted, reserves the chat's slot. A proactive send
// inside the window is declined (nothing is stamped); an explicit send always
// passes. Every admitted send stamps the chat, so a proactive sticker cannot
// follow right behind an explicit one.
func (l *stickerLimiter) admit(chatID int64, proactive bool, now time.Time, cooldown time.Duration) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if proactive {
		if at, ok := l.last[chatID]; ok {
			if wait := cooldown - now.Sub(at); wait > 0 {
				return false, wait
			}
		}
	}
	l.stamp(chatID, now, cooldown)

	return true, 0
}

// cooling reports, without reserving, whether a proactive send in the chat
// would currently be declined. It only decides advertisement; the dispatch
// check in admit stays authoritative.
func (l *stickerLimiter) cooling(chatID int64, now time.Time, cooldown time.Duration) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	at, ok := l.last[chatID]

	return ok && now.Sub(at) < cooldown
}

func (l *stickerLimiter) stamp(chatID int64, now time.Time, cooldown time.Duration) {
	if l.last == nil {
		l.last = make(map[int64]time.Time)
	}
	if _, known := l.last[chatID]; !known && len(l.last) >= stickerCooldownMaxEntries {
		var oldestChat int64
		var oldest time.Time
		first := true
		for chat, at := range l.last {
			if now.Sub(at) >= cooldown {
				delete(l.last, chat) // expired entries carry no information

				continue
			}
			if first || at.Before(oldest) {
				oldestChat, oldest, first = chat, at, false
			}
		}
		if len(l.last) >= stickerCooldownMaxEntries && !first {
			delete(l.last, oldestChat)
		}
	}
	l.last[chatID] = now
}

func (r *Runtime) stickerClock() time.Time {
	if r.stickerNow != nil {
		return r.stickerNow()
	}

	return time.Now()
}

// canSendSticker reports whether the run may still dispatch a sticker send:
// one admitted attempt per turn (failed and ambiguous ones included), and a
// bounded number of unknown-id misses.
func (s *runState) canSendSticker() bool {
	return s != nil && !s.stickerAttempted && s.stickerMisses < stickerMissLimit
}

// proactivelyCooling reports whether this run is proactive and its chat is
// inside the sticker cooldown.
func (r *Runtime) proactivelyCooling(call CallContext) bool {
	return call.run != nil && call.run.proactive &&
		r.stickerCooldown.cooling(call.Scope.ChatID, r.stickerClock(), stickerProactiveCooldown)
}

// sendStickerPresentation hides send_sticker whenever calling it cannot
// succeed: nothing active, the turn's attempt used up, too many misses, or a
// proactive turn inside the cooldown.
func (r *Runtime) sendStickerPresentation(call CallContext, p Presentation) (Presentation, bool) {
	if p, ok := r.stickersPresentation(call, p); !ok {
		return p, false
	}
	if call.run != nil && !call.run.canSendSticker() {
		return p, false
	}

	return p, true
}

func stickerSendError(code, message, hint string, class ErrorClass, retryable bool) toolResult {
	return ErrorResult(NewToolError(code, class, message, retryable, hint))
}

func stickerCancelledResult() toolResult {
	return stickerSendError("cancelled", "The sticker was cancelled before it was sent.", "", ErrorCancelled, false)
}

func (r *Runtime) sendSticker(ctx context.Context, callCtx CallContext, args json.RawMessage) (toolResult, error) {
	var payload struct {
		ID string `json:"id"`
	}
	if !decodeToolJSON(args, &payload) || payload.ID == "" {
		return toolResult{}, invalidArguments(`Provide the sticker as a JSON object with one "id" field taken from list_stickers.`)
	}
	run := callCtx.run
	if run == nil {
		return stickerSendError("run_state_unavailable", "Sticker state is unavailable for this call.", "", ErrorUnavailable, false), nil
	}

	// The turn's single attempt is checked before the id: a blocked turn
	// must not be coaxed into guessing.
	if run.stickerAttempted {
		return stickerSendError("sticker_limit_reached", "This turn already used its sticker send.", "Reply with text or say nothing instead.", ErrorPermanent, false), nil
	}
	if run.stickerMisses >= stickerMissLimit {
		return stickerSendError("sticker_lookup_limit", "Too many unknown sticker ids in this turn.", "Use list_stickers or reply without a sticker.", ErrorPermanent, false), nil
	}

	var index *stickers.Index
	if r.stickers != nil {
		index = r.stickers.Snapshot()
	}
	entry, found := index.Resolve(payload.ID)
	wellFormed := stickers.ValidateSemanticID(payload.ID) == nil
	if !wellFormed || !found {
		run.stickerMisses++

		return stickerSendError("unknown_sticker", "No active sticker has this id.", "Use an id returned by list_stickers.", ErrorNotFound, true), nil
	}
	if r.actions == nil {
		return stickerSendError("sender_unavailable", "Sticker sending is not available.", "", ErrorUnavailable, false), nil
	}

	// Nothing that can decline or cancel the call may sit between the
	// admission below and the send: a reserved cooldown slot must always be
	// followed by a real dispatch attempt.
	if contextDone(ctx) {
		return stickerCancelledResult(), nil
	}
	if admitted, wait := r.stickerCooldown.admit(callCtx.Scope.ChatID, run.proactive, r.stickerClock(), stickerProactiveCooldown); !admitted {
		declined := NewToolError("sticker_cooldown", ErrorRateLimited, "A sticker was sent in this chat moments ago.", false, "Reply with text or say nothing instead.")
		declined.RetryAfterSeconds = int((wait + time.Second - 1) / time.Second)

		return ErrorResult(declined), nil
	}

	params := tu.Sticker(tu.ID(callCtx.Scope.ChatID), tu.FileFromID(entry.FileID))
	if callCtx.Scope.TopicID != 0 {
		params.MessageThreadID = callCtx.Scope.TopicID
	}
	run.stickerAttempted = true
	message, err := r.actions.SendSticker(ctx, params)
	if err != nil {
		// One attempt per turn: whatever the transport said, no retry.
		classified := ClassifyError(err, true)
		classified.Retryable = false
		result := ErrorResult(classified)
		logToolResult(callCtx, result, "sticker_set_known", true)

		return result, nil
	}
	if message == nil || message.MessageID <= 0 {
		// A "successful" response without a message id confirms nothing.
		result := stickerSendError("invalid_send_response", "Telegram's answer did not confirm the sticker.", "Do not repeat the send without verifying its outcome.", ErrorAmbiguous, false)
		logToolResult(callCtx, result)

		return result, nil
	}

	result := toolResult{
		Status:  "ok",
		Summary: "Sticker sent",
		Data: map[string]any{
			"id":         entry.SemanticID,
			"chat_id":    callCtx.Scope.ChatID,
			"topic_id":   callCtx.Scope.TopicID,
			"message_id": message.MessageID,
		},
	}
	logToolResult(callCtx, result, "message_id", message.MessageID)

	return result, nil
}
