package bot

import (
	"log/slog"
	"time"

	"telegram-ollama-reply-bot/internal/state"
)

// reactionChange is one authorized, tracked reaction change in normalized
// form: the ingress adapter's output and the history processor's input. It
// is decoupled from Telegram update shapes so the processing logic is
// testable on plain data.
//
// reactions is the actor's complete tracked (whitelist-filtered) set after
// the change; an empty slice removes the actor's reactions. updateSeq is
// the sequential Telegram update ID of the update the change arrived in:
// the ordering authority for stale-update rejection in the store, which
// rejects anything not newer per user and target message.
type reactionChange struct {
	chatID    int64
	messageID int
	userID    int64
	name      string
	username  string
	updateSeq int64
	at        time.Time
	reactions []state.Reaction
}

// applyReactionChange processes one normalized reaction change against
// conversation history. It is the single authoritative state-mutation
// path for reactions: the change is applied synchronously here, and
// neither event delivery nor subscriber behavior can affect the outcome.
//
// The change is held in the pending buffer BEFORE the store lookup: if the
// lookup misses because the target reply's history insertion is racing
// this call, the hold still precedes that insertion's pending-update
// drain, so the change is replayed instead of stranded after it. Holding
// strictly before the lookup closes the window: whatever the
// interleaving, the change is either replayed by the drain or found live
// by this lookup.
//
// It reports true when the store resolved the change against an existing
// message — applied, or rejected as stale. The store returns true for both
// outcomes and false only when the target message is not in history, so
// the return value must not be read as confirmation of an actual state
// change; it only distinguishes "resolved" from "hold for replay".
func (b *Bot) applyReactionChange(change reactionChange, logger *slog.Logger) bool {
	key := pendingReactionKey{chatID: change.chatID, messageID: change.messageID}
	b.pendingReactions.hold(key, change.userID, change.updateSeq, change.reactions)

	if b.history.SetMessageReactions(change.chatID, change.messageID, change.userID, change.updateSeq, change.reactions) {
		// Applied, or rejected as stale against an existing message: drop
		// the hold so it does not linger until the TTL. If the insertion
		// drained it concurrently, release is a no-op and the sequence guard
		// made that replay idempotent.
		b.pendingReactions.release(key, change.userID, change.updateSeq)

		return true
	}

	logger.Debug("holding reaction for reply not yet in history")

	return false
}
