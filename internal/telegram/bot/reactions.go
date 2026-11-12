package bot

import (
	"slices"
	"strings"
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
// Reaction updates carry no Message payload, so the shared middlewares
// (including the access gate) never run for them — the gate is enforced here.
func (b *Bot) messageReactionHandler(ctx *th.Context, reaction t.MessageReactionUpdated) error {
	logger := b.handlerLogger(ctx).With("chat_id", reaction.Chat.ID, "message_id", reaction.MessageID)

	if reaction.User == nil {
		// Anonymous reactions (on behalf of a chat) arrive as
		// MessageReactionCount updates in group chats; User == nil here is a
		// defensive leftover of that path and carries no per-user feedback.
		logger.Debug("ignoring anonymous reaction")

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

	if !b.history.SetMessageReactions(reaction.Chat.ID, reaction.MessageID, reaction.User.ID, tracked) {
		logger.Debug("reaction target not in bot history")
	}

	return nil
}

// isAdminReactionDM mirrors isAdminDM for reaction updates, whose actor is a
// User rather than a message sender.
func (b *Bot) isAdminReactionDM(reaction t.MessageReactionUpdated) bool {
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
