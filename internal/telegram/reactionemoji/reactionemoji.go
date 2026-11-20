// Package reactionemoji holds the canonical whitelist of reaction emoji the
// bot works with. It is shared by the inbound reaction pipeline (which
// filters tracked feedback into history) and the model-facing set_reaction
// tool (which advertises the sendable subset as outgoing reaction
// candidates), so both sides always refer to one definition.
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
	"👍", "👎", "❤", "😂", "🤣", "😭", "😴", "💯",
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

// excludedFromReactions lists tracked emoji that Telegram does not offer as
// reaction choices at all (neither to users nor to bots), so they can never
// appear in incoming updates and must not be advertised as outgoing
// reaction candidates — sending one is rejected outright and would burn a
// turn's reaction attempt budget. They stay tracked above so a Telegram-side
// extension of the reaction list needs no code change beyond removing an
// entry here.
var excludedFromReactions = map[string]struct{}{
	// 😂 (face with tears of joy) and 🥳 (partying face) are not in the
	// ReactionTypeEmoji emoji list of the Bot API (see the pinned list in
	// the telego ReactionTypeEmoji documentation); 🤣 and 🎉 cover their
	// sentiment.
	"😂": {},
	"🥳": {},
}

// ReactionCandidates returns the tracked emoji that are valid Telegram
// reactions, in canonical order. This is the set advertised to the model for
// outgoing set_reaction calls; the full list remains the inbound filter.
func ReactionCandidates() []string {
	candidates := make([]string, 0, len(supported))
	for _, emoji := range supported {
		if _, excluded := excludedFromReactions[emoji]; !excluded {
			candidates = append(candidates, emoji)
		}
	}

	return candidates
}

// Candidate reports whether the exact emoji string may be used as an
// outgoing reaction.
func Candidate(emoji string) bool {
	if !Supported(emoji) {
		return false
	}

	_, excluded := excludedFromReactions[emoji]

	return !excluded
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

// CandidatesJoined renders the outgoing reaction candidates as a single
// space-separated string.
func CandidatesJoined() string {
	return strings.Join(ReactionCandidates(), " ")
}

// NormalizeCandidate accepts surrounding whitespace and equivalent Unicode
// presentation selectors. It never maps a different emoji, modifier or sequence
// onto a supported candidate; inbound feedback matching remains unchanged.
func NormalizeCandidate(value string) (string, bool) {
	value = strings.TrimSpace(value)
	if Candidate(value) {
		return value, true
	}
	canonical := strings.TrimSuffix(strings.TrimSuffix(value, "\ufe0f"), "\ufe0e")
	if canonical == value || (value != canonical+"\ufe0f" && value != canonical+"\ufe0e") {
		return "", false
	}

	return canonical, Candidate(canonical)
}
