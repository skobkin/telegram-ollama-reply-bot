package stickers

import (
	"strings"
	"testing"
)

func TestParseSetReference(t *testing.T) {
	good := map[string]string{
		"KitsuStickers":                                  "KitsuStickers",
		"  KitsuStickers \n":                             "KitsuStickers",
		"t.me/addstickers/KitsuStickers":                 "KitsuStickers",
		"https://t.me/addstickers/KitsuStickers":         "KitsuStickers",
		"http://t.me/addstickers/KitsuStickers/":         "KitsuStickers",
		"HTTPS://T.ME/addstickers/Kitsu_by_bot?x=1#frag": "Kitsu_by_bot",
		"https://telegram.me/addstickers/Kitsu":          "Kitsu",
		"https://www.t.me/addstickers/Kitsu":             "Kitsu",
		"tg://addstickers?set=Kitsu":                     "Kitsu",
		strings.Repeat("a", 64):                          strings.Repeat("a", 64),
	}
	for in, want := range good {
		if got, err := ParseSetReference(in); err != nil || got != want {
			t.Errorf("ParseSetReference(%q) = %q, %v; want %q", in, got, err, want)
		}
	}

	for _, in := range []string{
		"", "   ", "bad-name", "has space", strings.Repeat("a", 65),
		"https://t.me/addemoji/Kitsu", "https://t.me/Kitsu", "https://example.com/addstickers/Kitsu",
		"tg://resolve?domain=Kitsu", "tg://addstickers?set=", "tg://addstickers?set=bad-name", "t.me/addstickers/",
	} {
		if got, err := ParseSetReference(in); err == nil {
			t.Errorf("ParseSetReference(%q) = %q; expected an error", in, got)
		}
	}
}

func TestValidateSemanticID(t *testing.T) {
	for _, id := range []string{"abc", "kitsu_happy", "a" + strings.Repeat("1", 47)} {
		if err := ValidateSemanticID(id); err != nil {
			t.Errorf("%q must be valid: %v", id, err)
		}
	}
	for _, id := range []string{"", "ab", "1abc", "_abc", "Abc", "ab-c", "ab c", "привет", "a" + strings.Repeat("1", 48)} {
		if err := ValidateSemanticID(id); err == nil {
			t.Errorf("%q must be invalid", id)
		}
	}
}

func TestValidateDescriptionAndTags(t *testing.T) {
	if err := ValidateDescription("Привет, мир — 🦊"); err != nil {
		t.Fatalf("unicode description: %v", err)
	}
	for _, d := range []string{"a\nb", "a\tb", "a b", "a\x00b", "a\xffb"} {
		if err := ValidateDescription(d); err == nil {
			t.Errorf("%q must be rejected", d)
		}
	}
	if err := ValidateTags([]string{"счастье", "Happy"}); err != nil {
		t.Fatalf("multilingual tags: %v", err)
	}
	for _, tags := range [][]string{{""}, {"a\nb"}, {"Ab", "aB"}} {
		if err := ValidateTags(tags); err == nil {
			t.Errorf("%q must be rejected", tags)
		}
	}
}

func TestSanitizeTitle(t *testing.T) {
	if got := SanitizeTitle("  My\nsticker pack\t "); got != "My sticker pack" {
		t.Fatalf("got %q", got)
	}
	if got := SanitizeTitle(strings.Repeat("я", MaxTitleRunes+10)); len([]rune(got)) != MaxTitleRunes {
		t.Fatalf("title must be capped, got %d runes", len([]rune(got)))
	}
}

func TestValidateStateRejectsCorruptCatalogsWithMetadataOnlyMessages(t *testing.T) {
	good := func() State {
		return State{
			Sets:    []Set{{Name: "Kitsu", Title: "K", Enabled: true, Revision: 1}},
			Entries: []Entry{{SetName: "Kitsu", FileUniqueID: "a", FileID: "fa", SemanticID: "id_a", Description: "d", Format: FormatStatic, Position: 1, Available: true, Enabled: true}},
		}
	}
	if err := ValidateState(good()); err != nil {
		t.Fatalf("control: %v", err)
	}

	mutations := map[string]func(*State){
		"orphan entry":   func(s *State) { s.Entries[0].SetName = "Nowhere" },
		"duplicate row":  func(s *State) { s.Entries = append(s.Entries, s.Entries[0]) },
		"unknown format": func(s *State) { s.Entries[0].Format = "gif" },
		"invalid id":     func(s *State) { s.Entries[0].SemanticID = "Bad ID" },
		"oversized desc": func(s *State) { s.Entries[0].Description = strings.Repeat("x", MaxDescriptionRunes+1) },
		"empty file_id":  func(s *State) { s.Entries[0].FileID = "" },
		"negative pos":   func(s *State) { s.Entries[0].Position = -1 },
		"duplicate set":  func(s *State) { s.Sets = append(s.Sets, s.Sets[0]) },
		"bad set name":   func(s *State) { s.Sets[0].Name, s.Entries[0].SetName = "bad-name", "bad-name" },
		"duplicate id": func(s *State) {
			e := s.Entries[0]
			e.FileUniqueID = "b"
			s.Entries = append(s.Entries, e)
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			s := good()
			s.Entries[0].Description = "TOPSECRET"
			mutate(&s)
			err := ValidateState(s)
			if err == nil {
				t.Fatalf("expected %s to be rejected", name)
			}
			if strings.Contains(err.Error(), "TOPSECRET") {
				t.Fatalf("message leaked annotation text: %v", err)
			}
		})
	}
}

func TestValidateStateCaps(t *testing.T) {
	entries := func(n int, available bool) []Entry {
		out := make([]Entry, n)
		for i := range out {
			out[i] = Entry{SetName: "Kitsu", FileUniqueID: strings.Repeat("u", 1) + string(rune('A'+i%26)) + strings.Repeat("x", i/26), FileID: "f", Format: FormatStatic, Available: available, Enabled: true}
		}

		return out
	}
	set := []Set{{Name: "Kitsu", Title: "K", Enabled: true}}

	for name, tc := range map[string]struct {
		n         int
		available bool
		ok        bool
	}{
		"available at cap":   {MaxStickersPerSet, true, true},
		"available over cap": {MaxStickersPerSet + 1, true, false},
		"missing at cap":     {MaxMissingPerSet, false, true},
		"missing over cap":   {MaxMissingPerSet + 1, false, false},
	} {
		t.Run(name, func(t *testing.T) {
			err := ValidateState(State{Sets: set, Entries: entries(tc.n, tc.available)})
			if (err == nil) != tc.ok {
				t.Fatalf("n=%d available=%v: err=%v, want ok=%v", tc.n, tc.available, err, tc.ok)
			}
		})
	}

	total := State{}
	for i := 0; i < 30; i++ {
		name := "S" + string(rune('a'+i%26)) + string(rune('a'+i/26))
		total.Sets = append(total.Sets, Set{Name: name, Title: "t", Enabled: true})
		for j := 0; j < MaxStickersPerSet && len(total.Entries) <= MaxTotalEntries; j++ {
			total.Entries = append(total.Entries, Entry{SetName: name, FileUniqueID: string(rune('a'+j%26)) + strings.Repeat("z", j/26), FileID: "f", Format: FormatStatic, Available: true, Enabled: true})
		}
	}
	if len(total.Entries) > MaxTotalEntries && ValidateState(total) == nil {
		t.Fatalf("a catalog above %d entries must be rejected", MaxTotalEntries)
	}
}
