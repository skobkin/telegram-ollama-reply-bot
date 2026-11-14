// Package reactionemoji holds the canonical whitelist of reaction emoji the
// bot works with. It is shared by the inbound reaction pipeline (which
// filters tracked feedback into history) and the model-facing set_reaction
// tool (which validates and advertises the same set), so both sides always
// refer to one definition.
//
// Everything else is ignored — unlisted emoji, premium custom emoji, and
// paid reaction types. Telegram's per-chat reaction availability acts as the
// outer whitelist; this list only filters what reaches history, the model
// context, and outgoing model-authored reactions.
//
// Entries must be the exact strings Telegram transmits (see the reaction
// emoji list in the Bot API docs): notably the heart is U+2764 without the
// variation selector ("❤", not "❤️") and the bolt is U+26A1 ("⚡"). A typo here
// simply never matches — there is no startup validation on purpose, so the
// list needs no coupling to Telegram's current emoji set.
package reactionemoji

import "strings"

// supported is the canonical, deterministically ordered whitelist. Do not
// reorder casually: the order is user-visible in the set_reaction tool
// description and its JSON Schema enum.
var supported = []string{
	// Best clarity: unambiguous sentiment the model reads directly.
	"👍", "👎", "❤", "😂", "😭", "😴", "💯",
	"🏆", "⚡", "🙏", "🤝", "👏", "👌",

	// Good clarity: reliably understood in conversational context.
	"🔥", "🎉", "🤔", "👀", "😐", "🤨", "😎",
	"🥰", "🤩", "😍", "🥳", "🫡", "🤯", "😱",
	"💔", "😈", "🤡", "🤓",

	// Contextual: usually clear from context, but can be trimmed here if
	// they start misleading the model.
	"🥴", "🥱", "🤮", "💩",
}

// lookup is derived from supported; membership checks use it directly.
var lookup = func() map[string]struct{} {
	set := make(map[string]struct{}, len(supported))
	for _, emoji := range supported {
		set[emoji] = struct{}{}
	}

	return set
}()

// All returns a copy of the canonical ordered emoji list.
func All() []string {
	result := make([]string, len(supported))
	copy(result, supported)

	return result
}

// Supported reports whether the exact emoji string is on the whitelist.
// Comparison is byte-exact: "❤️" (with the U+FE0F variation selector) is not
// supported, "❤" (U+2764 alone) is.
func Supported(emoji string) bool {
	_, ok := lookup[emoji]

	return ok
}

// Joined renders the whitelist as a single space-separated string for
// model-visible tool descriptions.
func Joined() string {
	return strings.Join(supported, " ")
}
