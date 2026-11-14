package reactionemoji

import (
	"slices"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestAllHasExactTelegramForms(t *testing.T) {
	all := All()
	if len(all) == 0 {
		t.Fatal("whitelist must not be empty")
	}

	for _, emoji := range all {
		if !utf8.ValidString(emoji) {
			t.Errorf("entry %q is not valid UTF-8", emoji)
		}
		if emoji == "" {
			t.Error("empty whitelist entry")
		}
		if strings.ContainsRune(emoji, '️') {
			t.Errorf("whitelist entry %q contains a variation selector; Telegram sends plain codepoints", emoji)
		}
	}

	// The heart must be U+2764 without the U+FE0F variation selector.
	if !slices.Contains(all, "❤") {
		t.Error("whitelist must contain the bare heart U+2764")
	}
	if slices.Contains(all, "❤️") {
		t.Error("whitelist must not contain the heart with variation selector U+FE0F")
	}

	// The bolt must be U+26A1 without a variation selector.
	if !slices.Contains(all, "⚡") {
		t.Error("whitelist must contain the bolt U+26A1")
	}
	if slices.Contains(all, "⚡️") {
		t.Error("whitelist must not contain the bolt with variation selector U+FE0F")
	}

	// The rolling-on-the-floor-laughing face must be tracked: it is the
	// laughter reaction Telegram actually offers (see excludedFromReactions).
	if !slices.Contains(all, "🤣") {
		t.Error("whitelist must contain rolling on the floor laughing U+1F923")
	}

	// Duplicates would silently skew Joined() and the schema enum.
	seen := make(map[string]struct{}, len(all))
	for _, emoji := range all {
		if _, dup := seen[emoji]; dup {
			t.Errorf("duplicate whitelist entry %q", emoji)
		}
		seen[emoji] = struct{}{}
	}
}

func TestAllReturnsDefensiveCopy(t *testing.T) {
	all := All()
	if len(all) == 0 {
		t.Fatal("whitelist must not be empty")
	}

	all[0] = "tampered"
	if Supported("tampered") {
		t.Error("mutating All() must not affect the underlying whitelist")
	}
	if All()[0] == "tampered" {
		t.Error("All() must return a copy, not the backing slice")
	}
}

func TestSupportedExactMembership(t *testing.T) {
	for _, emoji := range All() {
		if !Supported(emoji) {
			t.Errorf("Supported(%q) = false for a whitelisted emoji", emoji)
		}
	}

	for _, emoji := range []string{"", "❤️", "😁", "👍🏻", "not-emoji"} {
		if Supported(emoji) {
			t.Errorf("Supported(%q) = true for a non-whitelisted value", emoji)
		}
	}
}

func TestJoinedDeterministic(t *testing.T) {
	first := Joined()
	second := Joined()
	if first == "" {
		t.Fatal("Joined() must not be empty")
	}
	if first != second {
		t.Fatal("Joined() must be deterministic")
	}

	if got, want := strings.Split(first, " "), All(); !slices.Equal(got, want) {
		t.Errorf("Joined() = %q, want space-joined All()", first)
	}
}

func TestReactionCandidatesAreTrackedSubset(t *testing.T) {
	t.Parallel()

	candidates := ReactionCandidates()
	if len(candidates) == 0 || len(candidates) >= len(All()) {
		t.Fatalf("candidates must be a proper non-empty subset: %d of %d", len(candidates), len(All()))
	}

	for _, emoji := range candidates {
		if !Supported(emoji) {
			t.Errorf("candidate %q must be tracked", emoji)
		}
		if !Candidate(emoji) {
			t.Errorf("candidate %q must report Candidate()=true", emoji)
		}
	}

	// Candidates stay in canonical order.
	ordered := make([]string, 0, len(All()))
	for _, emoji := range All() {
		if Candidate(emoji) {
			ordered = append(ordered, emoji)
		}
	}
	if !slices.Equal(candidates, ordered) {
		t.Error("ReactionCandidates() must preserve the canonical order of All()")
	}

	// Emoji Telegram does not offer as reactions are tracked but never
	// advertised as outgoing candidates (see excludedFromReactions).
	for _, emoji := range []string{"😂", "🥳"} {
		if !Supported(emoji) {
			t.Errorf("%q must stay tracked for inbound filtering", emoji)
		}
		if Candidate(emoji) {
			t.Errorf("%q must not be an outgoing reaction candidate", emoji)
		}
	}

	// 🤣 is the laughter reaction Telegram does offer, so it must be
	// advertised even though 😂 stays excluded.
	if !Supported("🤣") {
		t.Error("🤣 must be tracked")
	}
	if !Candidate("🤣") {
		t.Error("🤣 must be an outgoing reaction candidate")
	}

	// Membership follows the candidate list exactly.
	for _, emoji := range All() {
		if got, want := Candidate(emoji), slices.Contains(candidates, emoji); got != want {
			t.Errorf("Candidate(%q) = %v, want %v", emoji, got, want)
		}
	}
	if Candidate("") || Candidate("❤️") || Candidate("😁") {
		t.Error("unknown or invalid forms must not be candidates")
	}
}

func TestCandidatesJoinedDeterministic(t *testing.T) {
	t.Parallel()

	first := CandidatesJoined()
	if first == "" {
		t.Fatal("CandidatesJoined() must not be empty")
	}
	if first != CandidatesJoined() {
		t.Fatal("CandidatesJoined() must be deterministic")
	}
	if got, want := strings.Split(first, " "), ReactionCandidates(); !slices.Equal(got, want) {
		t.Errorf("CandidatesJoined() = %q, want space-joined ReactionCandidates()", first)
	}
}
