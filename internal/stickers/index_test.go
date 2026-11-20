package stickers

import (
	"strings"
	"testing"
)

func searchCatalog(t *testing.T) *Index {
	t.Helper()

	e := func(unique, id, desc, emoji string, pos int, tags ...string) Entry {
		x := annotated(unique, id, desc, tags...)
		x.Emoji, x.Position = emoji, pos

		return x
	}
	s := stateFrom(t,
		e("u1", "kitsu_happy", "Happy fox waving hello", "🦊", 1, "happy", "greeting", "привет"),
		e("u2", "kitsu_sad", "Sad fox crying", "😢", 2, "sad", "Грусть"),
		e("u3", "kitsu_hello", "Fox says hi", "👋", 3, "greeting"),
		e("u4", "kitsu_sleep", "Sleepy fox", "😴", 4),
	)
	// Entries that must never surface.
	s.Entries = append(s.Entries,
		Entry{SetName: "Kitsu", FileUniqueID: "u5", FileID: "f5", SemanticID: "kitsu_off", Description: "Disabled", Format: FormatStatic, Position: 5, Available: true, Enabled: false},
		Entry{SetName: "Kitsu", FileUniqueID: "u6", FileID: "f6", SemanticID: "kitsu_gone", Description: "Unavailable", Format: FormatStatic, Position: 6, Available: false, Enabled: true},
		Entry{SetName: "Kitsu", FileUniqueID: "u7", FileID: "f7", SemanticID: "kitsu_nodesc", Format: FormatStatic, Position: 7, Available: true, Enabled: true},
		Entry{SetName: "Kitsu", FileUniqueID: "u8", FileID: "f8", Description: "No id", Format: FormatStatic, Position: 8, Available: true, Enabled: true},
	)
	s.Sets = append(s.Sets, Set{Name: "Hidden", Title: "Hidden set", Enabled: false, Revision: 1})
	s.Entries = append(s.Entries, Entry{SetName: "Hidden", FileUniqueID: "h1", FileID: "fh", SemanticID: "hidden_one", Description: "In a disabled set", Format: FormatStatic, Position: 1, Available: true, Enabled: true})
	if err := ValidateState(s); err != nil {
		t.Fatalf("catalog: %v", err)
	}

	return BuildIndex(s)
}

func ids(hits []Hit) string {
	out := make([]string, len(hits))
	for i, h := range hits {
		out[i] = h.ID
	}

	return strings.Join(out, ",")
}

func TestIndexContainsOnlyActiveEntries(t *testing.T) {
	idx := searchCatalog(t)
	if idx.Len() != 4 {
		t.Fatalf("expected 4 active entries, got %d", idx.Len())
	}
	for _, id := range []string{"kitsu_off", "kitsu_gone", "kitsu_nodesc", "hidden_one"} {
		if _, ok := idx.Resolve(id); ok {
			t.Errorf("%s must not resolve", id)
		}
	}
	if e, ok := idx.Resolve("kitsu_happy"); !ok || e.FileID != "file-u1" {
		t.Fatalf("active entry must resolve to its send token: %+v %v", e, ok)
	}
	if _, ok := (*Index)(nil).Resolve("x"); ok || (*Index)(nil).Len() != 0 {
		t.Fatalf("a nil index is empty")
	}
}

func TestSearchEmptyQueryIsStableAndReportsTotal(t *testing.T) {
	idx := searchCatalog(t)

	hits, total := idx.Search("", nil, 2)
	if ids(hits) != "kitsu_happy,kitsu_sad" || total != 4 {
		t.Fatalf("empty query must list in set/position order: %s total=%d", ids(hits), total)
	}
	hits, _ = idx.Search("   ", nil, 0)
	if len(hits) != 4 {
		t.Fatalf("a default limit applies when none is given, got %d", len(hits))
	}
}

func TestSearchRankingPolicy(t *testing.T) {
	idx := searchCatalog(t)

	cases := []struct{ name, query, want string }{
		{"exact id wins", "kitsu_sad", "kitsu_sad,kitsu_happy,kitsu_hello,kitsu_sleep"}, // others match only the shared "kitsu" id token
		{"tag beats description word", "greeting", "kitsu_happy,kitsu_hello"},
		{"id token", "sleep", "kitsu_sleep"},
		{"description word", "crying", "kitsu_sad"},
		{"emoji field", "👋", "kitsu_hello"},
		{"emoji with variation selector", "😴️", "kitsu_sleep"},
		{"case-folded tag", "HAPPY", "kitsu_happy"},
		{"cyrillic tag", "ПРИВЕТ", "kitsu_happy"},
		{"cyrillic tag mixed case", "грусть", "kitsu_sad"},
		{"multi token any match", "sad hello", "kitsu_sad,kitsu_hello,kitsu_happy"},
		{"no match", "dragon", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hits, _ := idx.Search(tc.query, nil, 10)
			if got := ids(hits); got != tc.want {
				t.Fatalf("query %q: got %q, want %q", tc.query, got, tc.want)
			}
		})
	}
}

func TestSearchTieBreaksByPositionAndIsDeterministic(t *testing.T) {
	idx := searchCatalog(t)
	// "fox" appears once in every description: equal scores for all four
	// except where it is also an id token.
	first, _ := idx.Search("fox", nil, 10)
	for i := 0; i < 5; i++ {
		again, _ := idx.Search("fox", nil, 10)
		if ids(again) != ids(first) {
			t.Fatalf("ranking must be deterministic")
		}
	}
	if ids(first) != "kitsu_happy,kitsu_sad,kitsu_hello,kitsu_sleep" {
		t.Fatalf("equal scores must order by position, got %s", ids(first))
	}
}

func TestSearchTagFilterIsAnAndFilter(t *testing.T) {
	idx := searchCatalog(t)

	hits, total := idx.Search("", []string{"Greeting"}, 10)
	if ids(hits) != "kitsu_happy,kitsu_hello" || total != 2 {
		t.Fatalf("tag filter: %s total=%d", ids(hits), total)
	}
	hits, _ = idx.Search("", []string{"greeting", "happy"}, 10)
	if ids(hits) != "kitsu_happy" {
		t.Fatalf("every filter tag must match: %s", ids(hits))
	}
	if hits, _ = idx.Search("", []string{"greeting", "sad"}, 10); len(hits) != 0 {
		t.Fatalf("no entry has both tags")
	}
	hits, _ = idx.Search("hello", []string{"greeting"}, 10)
	if ids(hits) != "kitsu_hello,kitsu_happy" {
		t.Fatalf("query and filter combine: %s", ids(hits))
	}
}

func TestSearchLimitIsClampedAndHitsCarryNoTokens(t *testing.T) {
	idx := searchCatalog(t)

	if hits, total := idx.Search("", nil, 1); len(hits) != 1 || total != 4 {
		t.Fatalf("limit 1: %d hits, total %d", len(hits), total)
	}
	if hits, _ := idx.Search("", nil, 1000); len(hits) != 4 {
		t.Fatalf("an oversized limit is clamped, not an error")
	}
	hits, _ := idx.Search("kitsu_happy", nil, 1)
	h := hits[0]
	if h.SetTitle != "KITSU" || h.Emoji != "🦊" || len(h.Tags) != 3 || h.Description == "" {
		t.Fatalf("hit lacks presentation data: %+v", h)
	}
	h.Tags[0] = "mutated"
	again, _ := idx.Search("kitsu_happy", nil, 1)
	if again[0].Tags[0] == "mutated" {
		t.Fatalf("hits must not alias the index's data")
	}
}

func TestIndexSnapshotsAreIndependent(t *testing.T) {
	stored := baseCatalog(t)
	old := BuildIndex(stored)

	plan := mustPlan(t, stored, tmplOf("Kitsu", TemplateSticker{FileUniqueID: "a", Reset: boolp(true)}), remoteSet("Kitsu", "a", "b", "c"))
	next := BuildIndex(plan.Next)

	if _, ok := old.Resolve("id_a"); !ok {
		t.Fatalf("a reader holding the old snapshot must keep its view")
	}
	if _, ok := next.Resolve("id_a"); ok {
		t.Fatalf("the new snapshot must not contain the reset id")
	}
}
