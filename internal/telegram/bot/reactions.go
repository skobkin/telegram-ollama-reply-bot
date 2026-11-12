package bot

import (
	"cmp"
	"context"
	"slices"
	"strings"
	"sync"
	"time"

	"telegram-ollama-reply-bot/internal/state"

	t "github.com/mymmrac/telego"
	th "github.com/mymmrac/telego/telegohandler"
)

// trackedReactionEmojis is the hardcoded feedback whitelist: reactions whose
// emoji the backend model is known to read reliably. Everything else is
// ignored — unlisted emoji, premium custom emoji, and paid reaction types.
// Telegram's per-chat reaction availability acts as the outer whitelist; this
// list only filters what reaches history and the model context.
//
// Entries must be the exact strings Telegram transmits (see the reaction
// emoji list in the Bot API docs): notably the heart is U+2764 without the
// variation selector ("❤", not "❤️") and the bolt is U+26A1 ("⚡"). A typo here
// simply never matches — there is no startup validation on purpose, so the
// list needs no coupling to Telegram's current emoji set.
var trackedReactionEmojis = map[string]struct{}{
	// Best clarity: unambiguous sentiment the model reads directly.
	"👍": {}, "👎": {}, "❤": {}, "😂": {}, "😭": {}, "😴": {}, "💯": {},
	"🏆": {}, "⚡": {}, "🙏": {}, "🤝": {}, "👏": {}, "👌": {},

	// Good clarity: reliably understood in conversational context.
	"🔥": {}, "🎉": {}, "🤔": {}, "👀": {}, "😐": {}, "🤨": {}, "😎": {},
	"🥰": {}, "🤩": {}, "😍": {}, "🥳": {}, "🫡": {}, "🤯": {}, "😱": {},
	"💔": {}, "😈": {}, "🤡": {}, "🤓": {},

	// Contextual: usually clear from context, but can be trimmed here if
	// they start misleading the model.
	"🥴": {}, "🥱": {}, "🤮": {}, "💩": {},
}

// messageReactionHandler records reactions to the bot's own messages in
// conversation history so the model sees the feedback in later replies.
// It is registered as a generic route (not the typed HandleMessageReaction)
// so the update envelope's sequential UpdateID reaches the store for
// stale-update rejection: handlers run concurrently and do not preserve
// Telegram's delivery order. Reaction updates carry no Message payload, so
// the shared middlewares (including the access gate) never run for them —
// the gate is enforced here.
func (b *Bot) messageReactionHandler(ctx *th.Context, update t.Update) error {
	reaction := update.MessageReaction
	if reaction == nil {
		return nil
	}

	logger := b.handlerLogger(ctx).With("chat_id", reaction.Chat.ID, "message_id", reaction.MessageID)

	if reaction.User == nil {
		// Anonymous reactions (on behalf of a chat) arrive as
		// MessageReactionCount updates in group chats; User == nil here is a
		// defensive leftover of that path and carries no per-user feedback.
		logger.Debug("ignoring anonymous reaction")

		return nil
	}

	// The shared dmAdminsOnlyGate only inspects Message updates, so the
	// BOT__DM_ADMINS_ONLY restriction is re-enforced here for reaction
	// updates: a non-admin private reaction is dropped before the admin
	// service is consulted or history is touched.
	if b.cfg.DMAdminsOnly && reaction.Chat.Type == t.ChatTypePrivate && !b.isAdminReactionDM(reaction) {
		logger.Info("dropping non-admin private reaction (BOT__DM_ADMINS_ONLY)", "from_id", reaction.User.ID)

		return nil
	}

	if b.admin != nil {
		allowed, err := b.admin.ShouldAllowChat(ctx.Context(), reaction.Chat.ID, b.isAdminReactionDM(reaction))
		if err != nil {
			return err
		}
		if !allowed {
			logger.Info("ignoring reaction from non-whitelisted chat", "from_id", reaction.User.ID)

			return nil
		}
	}

	logger.Debug("processing reaction update",
		"from_id", reaction.User.ID,
		"old", reactionEmojisLogValue(reaction.OldReaction),
		"new", reactionEmojisLogValue(reaction.NewReaction))

	at := time.Now().UTC()
	if reaction.Date != 0 {
		at = time.Unix(reaction.Date, 0).UTC()
	}

	tracked := b.trackedReactions(reaction.NewReaction, reaction.User, at)
	if len(tracked) == 0 && !hasTrackedEmoji(reaction.OldReaction) {
		logger.Debug("reaction change not tracked")

		return nil
	}

	key := pendingReactionKey{chatID: reaction.Chat.ID, messageID: reaction.MessageID}
	seq := int64(update.UpdateID)

	// Hold before applying: if the store lookup misses because the target
	// reply's history insertion is racing this handler, the hold still
	// precedes that insertion's pending-update drain, so the update is
	// replayed instead of stranded after it. Holding strictly before the
	// lookup closes the window: whatever the interleaving, the update is
	// either replayed by the drain or found live by this lookup.
	b.pendingReactions.hold(key, reaction.User.ID, seq, tracked)

	if b.history.SetMessageReactions(reaction.Chat.ID, reaction.MessageID, reaction.User.ID, seq, tracked) {
		// Applied, or rejected as stale against an existing message: drop
		// the hold so it does not linger until the TTL. If the insertion
		// drained it concurrently, release is a no-op and the sequence guard
		// made that replay idempotent.
		b.pendingReactions.release(key, reaction.User.ID, seq)

		return nil
	}

	logger.Debug("holding reaction for reply not yet in history")

	return nil
}

const (
	// pendingReactionTTL bounds how long a reaction update is held while its
	// target message may still be in flight. It is generous next to the real
	// send latency so no live update is lost; holds whose target never gets
	// recorded (user messages, history lost to restart or trim) expire
	// silently.
	pendingReactionTTL = 15 * time.Second

	// pendingReactionMaxEntries caps the buffer against bursts of reactions
	// to messages that will never be recorded; the oldest hold is dropped
	// first. A popular fresh reply can legitimately hold many updates at
	// once, so the cap applies globally rather than per message.
	pendingReactionMaxEntries = 128
)

// pendingReactionKey identifies one Telegram message; reaction updates carry
// no topic, matching the store's chat-wide lookup.
type pendingReactionKey struct {
	chatID    int64
	messageID int
}

// pendingReactionUpdate is one held reaction update, replayed in update
// order once its target message is recorded in history.
type pendingReactionUpdate struct {
	userID    int64
	updateSeq int64
	reactions []state.Reaction
	heldAt    time.Time
}

// pendingReactionBuffer holds reaction updates for bot replies whose send
// response has not landed yet. The zero value is ready to use so minimally
// constructed bots keep working.
type pendingReactionBuffer struct {
	mu      sync.Mutex
	entries map[pendingReactionKey][]pendingReactionUpdate
}

// hold buffers one reaction update, pruning expired holds and keeping the
// buffer within its entry cap.
func (p *pendingReactionBuffer) hold(key pendingReactionKey, userID, updateSeq int64, reactions []state.Reaction) {
	p.mu.Lock()
	defer p.mu.Unlock()

	now := time.Now().UTC()
	p.pruneLocked(now)
	if p.countLocked() >= pendingReactionMaxEntries {
		p.dropOldestLocked(now)
	}
	if p.entries == nil {
		p.entries = make(map[pendingReactionKey][]pendingReactionUpdate)
	}
	p.entries[key] = append(p.entries[key], pendingReactionUpdate{
		userID:    userID,
		updateSeq: updateSeq,
		reactions: reactions,
		heldAt:    now,
	})
}

// take removes and returns the held updates for one message in update order:
// handlers run concurrently, so the held order is arrival order.
func (p *pendingReactionBuffer) take(key pendingReactionKey) []pendingReactionUpdate {
	p.mu.Lock()
	defer p.mu.Unlock()

	held := p.entries[key]
	if len(held) == 0 {
		return nil
	}
	delete(p.entries, key)

	slices.SortFunc(held, func(a, b pendingReactionUpdate) int {
		return cmp.Compare(a.updateSeq, b.updateSeq)
	})

	return held
}

// release drops a held update that was applied (or superseded) live, so it
// does not linger until the TTL. No-op when the hold is already gone, e.g.
// drained by a concurrent history insertion.
func (p *pendingReactionBuffer) release(key pendingReactionKey, userID, updateSeq int64) {
	p.mu.Lock()
	defer p.mu.Unlock()

	held := p.entries[key]
	for i, update := range held {
		if update.userID == userID && update.updateSeq == updateSeq {
			held = slices.Delete(held, i, i+1)

			break
		}
	}
	if len(held) == 0 {
		delete(p.entries, key)

		return
	}
	p.entries[key] = held
}

func (p *pendingReactionBuffer) pruneLocked(now time.Time) {
	for key, held := range p.entries {
		kept := held[:0]
		for _, update := range held {
			if now.Sub(update.heldAt) < pendingReactionTTL {
				kept = append(kept, update)
			}
		}
		if len(kept) == 0 {
			delete(p.entries, key)

			continue
		}
		p.entries[key] = kept
	}
}

func (p *pendingReactionBuffer) countLocked() int {
	total := 0
	for _, held := range p.entries {
		total += len(held)
	}

	return total
}

func (p *pendingReactionBuffer) dropOldestLocked(now time.Time) {
	var (
		oldestKey   pendingReactionKey
		oldestIndex = -1
		oldestAt    = now
	)
	for key, held := range p.entries {
		for i, update := range held {
			if oldestIndex < 0 || update.heldAt.Before(oldestAt) {
				oldestKey, oldestIndex, oldestAt = key, i, update.heldAt
			}
		}
	}
	if oldestIndex < 0 {
		return
	}

	held := slices.Delete(p.entries[oldestKey], oldestIndex, oldestIndex+1)
	if len(held) == 0 {
		delete(p.entries, oldestKey)

		return
	}
	p.entries[oldestKey] = held
}

// replayPendingReactions applies held reaction updates once the reply they
// target is recorded in history, in Telegram update order, so reactions that
// arrived during the send response are neither dropped nor reordered.
func (b *Bot) replayPendingReactions(ctx context.Context, chatID int64, messageID int) {
	held := b.pendingReactions.take(pendingReactionKey{chatID: chatID, messageID: messageID})
	if len(held) == 0 {
		return
	}

	logger := b.loggerFromContext(ctx).With("chat_id", chatID, "message_id", messageID)
	logger.Debug("replaying reactions held during send", "updates", len(held))

	for _, update := range held {
		if !b.history.SetMessageReactions(chatID, messageID, update.userID, update.updateSeq, update.reactions) {
			logger.Debug("held reaction target vanished before replay",
				"user_id", update.userID, "update_seq", update.updateSeq)
		}
	}
}

// isAdminReactionDM mirrors isAdminDM for reaction updates, whose actor is a
// User rather than a message sender.
func (b *Bot) isAdminReactionDM(reaction *t.MessageReactionUpdated) bool {
	return reaction.User != nil &&
		reaction.Chat.Type == t.ChatTypePrivate &&
		slices.Contains(b.cfg.AdminIDs, reaction.User.ID)
}

// trackedReactions converts a user's current reaction set into stored
// reactions, keeping only whitelisted emoji reactions. The list is the
// user's complete reaction state, so the result replaces their previous
// reactions wholesale.
func (b *Bot) trackedReactions(list []t.ReactionType, user *t.User, at time.Time) []state.Reaction {
	var tracked []state.Reaction

	for _, reactionType := range list {
		emoji, ok := reactionEmoji(reactionType)
		if !ok {
			continue
		}
		if _, ok := trackedReactionEmojis[emoji]; !ok {
			continue
		}

		tracked = append(tracked, state.Reaction{
			UserID:    user.ID,
			Name:      strings.TrimSpace(user.FirstName + " " + user.LastName),
			Username:  user.Username,
			Emoji:     emoji,
			UpdatedAt: at,
		})
	}

	return tracked
}

// reactionEmoji returns the emoji of a simple emoji reaction; custom (premium)
// and paid reaction types have no emoji and are never tracked.
func reactionEmoji(reactionType t.ReactionType) (string, bool) {
	emoji, ok := reactionType.(*t.ReactionTypeEmoji)
	if !ok {
		return "", false
	}

	return emoji.Emoji, true
}

func hasTrackedEmoji(list []t.ReactionType) bool {
	for _, reactionType := range list {
		if emoji, ok := reactionEmoji(reactionType); ok {
			if _, tracked := trackedReactionEmojis[emoji]; tracked {
				return true
			}
		}
	}

	return false
}

// reactionEmojisLogValue renders a reaction list for logging: emoji joined
// with commas, reaction type names for non-emoji entries, "-" when empty.
func reactionEmojisLogValue(list []t.ReactionType) string {
	if len(list) == 0 {
		return "-"
	}

	parts := make([]string, 0, len(list))
	for _, reactionType := range list {
		if emoji, ok := reactionEmoji(reactionType); ok {
			parts = append(parts, emoji)
		} else {
			parts = append(parts, reactionType.ReactionType())
		}
	}

	return strings.Join(parts, ",")
}
