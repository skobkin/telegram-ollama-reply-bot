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
