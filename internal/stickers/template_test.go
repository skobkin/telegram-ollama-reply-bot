package stickers

import (
	"fmt"
	"strings"
	"testing"
)

func TestBuildTemplateIsDeterministicAndBlankForNewSet(t *testing.T) {
	remote := remoteSet("Kitsu", "u1", "u2")

	a, err := BuildTemplate(State{}, remote)
	if err != nil {
		t.Fatalf("BuildTemplate: %v", err)
	}
	b, _ := BuildTemplate(State{}, remote)
	if string(a) != string(b) {
		t.Fatalf("export is not deterministic")
	}

	text := string(a)
	for _, want := range []string{"# Sticker catalog template", "schema_version: 1", "set: Kitsu", "file_unique_id: u1", "position: 2", "id: \"\"", "tags: []", "enabled: true", "reset: false"} {
		if !strings.Contains(text, want) {
			t.Fatalf("export lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "file-u1") {
		t.Fatalf("export must not carry file_id:\n%s", text)
	}
	if strings.Contains(text, "missing:") {
		t.Fatalf("a first export has no missing section:\n%s", text)
	}
}

func TestRepeatExportPrefillsAnnotationsRegardlessOfOrderAndLeavesNewBlank(t *testing.T) {
	stored := stateFrom(t,
		annotated("u1", "kitsu_happy", "Happy fox", "happy", "привет"),
		annotated("u2", "kitsu_sad", "Sad fox", "sad"),
		Entry{FileUniqueID: "u3", Available: true, Enabled: true},                                                     // unannotated
		Entry{FileUniqueID: "u4", SemanticID: "kitsu_gone", Description: "Gone fox", Available: false, Enabled: true}, // retained
	)
	stored.Entries[1].Enabled = false

	// Telegram now lists the set in a different order, with one new sticker.
	remote := remoteSet("Kitsu", "new1", "u2", "u3", "u1")

	raw, err := BuildTemplate(stored, remote)
	if err != nil {
		t.Fatalf("BuildTemplate: %v", err)
	}
	tmpl, err := ParseTemplate(raw)
	if err != nil {
		t.Fatalf("exported template must parse: %v\n%s", err, raw)
	}

	if got := len(tmpl.Stickers); got != 4 {
		t.Fatalf("expected 4 stickers, got %d", got)
	}
	byID := map[string]TemplateSticker{}
	for i, s := range tmpl.Stickers {
		byID[s.FileUniqueID] = s
		if s.Position != i+1 {
			t.Fatalf("position must follow the current Telegram order, got %d at index %d", s.Position, i)
		}
	}
	if s := byID["u1"]; s.ID != "kitsu_happy" || s.Description != "Happy fox" || strings.Join(s.Tags, ",") != "happy,привет" || !flag(s.Enabled) {
		t.Fatalf("u1 must be prefilled: %+v", s)
	}
	if s := byID["u2"]; s.ID != "kitsu_sad" || s.Enabled == nil || *s.Enabled {
		t.Fatalf("u2 must keep id and enabled=false: %+v", s)
	}
	for _, blank := range []string{"new1", "u3"} {
		if s := byID[blank]; s.ID != "" || s.Description != "" || len(s.Tags) != 0 || !flag(s.Enabled) || flag(s.Reset) {
			t.Fatalf("%s must be blank with defaults: %+v", blank, s)
		}
	}

	if len(tmpl.Missing) != 1 || tmpl.Missing[0].FileUniqueID != "u4" || tmpl.Missing[0].ID != "kitsu_gone" || flag(tmpl.Missing[0].Forget) {
		t.Fatalf("u4 must be disclosed under missing: %+v", tmpl.Missing)
	}
}

func TestExportImportRoundTripReproducesAnnotations(t *testing.T) {
	remote := remoteSet("Kitsu", "u1", "u2", "u3")
	raw, _ := BuildTemplate(State{}, remote)
	tmpl, err := ParseTemplate(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	tmpl.Stickers[0].ID, tmpl.Stickers[0].Description, tmpl.Stickers[0].Tags = "kitsu_happy", "Happy fox", []string{"happy", "hello"}
	tmpl.Stickers[2].ID, tmpl.Stickers[2].Description = "kitsu_zzz", `Sleepy: "zzz" #fox`

	plan := mustPlan(t, State{}, tmpl, remote)
	if plan.Report.Added != 3 || plan.Report.Incomplete != 1 {
		t.Fatalf("unexpected report: %+v", plan.Report)
	}

	again, err := BuildTemplate(plan.Next, remote)
	if err != nil {
		t.Fatalf("re-export: %v", err)
	}
	reparsed, err := ParseTemplate(again)
	if err != nil {
		t.Fatalf("parse re-export: %v\n%s", err, again)
	}
	for i, want := range tmpl.Stickers {
		got := reparsed.Stickers[i]
		if got.ID != want.ID || got.Description != want.Description || strings.Join(got.Tags, "|") != strings.Join(want.Tags, "|") {
			t.Fatalf("sticker %d lost its annotation: want %+v got %+v", i, want, got)
		}
	}

	// Re-importing the re-export is idempotent.
	second := mustPlan(t, plan.Next, reparsed, remote)
	if r := second.Report; r.Unchanged != 3 || r.Added+r.Updated+r.Reset != 0 {
		t.Fatalf("re-import must be idempotent: %+v", r)
	}
}

func TestParseTemplateRejections(t *testing.T) {
	valid := "schema_version: 1\nset: Kitsu\nstickers:\n  - file_unique_id: u1\n    id: kitsu_a\n"

	cases := map[string]string{
		"empty":            "",
		"invalid utf8":     "schema_version: 1\nset: \xff\xfe\nstickers: []\n",
		"unknown field":    valid + "    file_id: AAA\n",
		"unknown top":      valid + "extra: 1\n",
		"wrong schema":     strings.Replace(valid, "schema_version: 1", "schema_version: 2", 1),
		"missing schema":   strings.Replace(valid, "schema_version: 1\n", "", 1),
		"bad set name":     strings.Replace(valid, "set: Kitsu", "set: bad-name", 1),
		"alias":            "schema_version: 1\nset: Kitsu\nstickers:\n  - &a {file_unique_id: u1}\n  - *a\n",
		"duplicate key":    valid + "    id: kitsu_b\n",
		"two documents":    valid + "---\n" + valid,
		"scalar tags":      valid + "    tags: happy\n",
		"not a mapping":    "- a\n- b\n",
		"broken yaml":      "schema_version: [1\n",
		"too many entries": "schema_version: 1\nset: Kitsu\nstickers:\n" + strings.Repeat("  - file_unique_id: x\n", MaxStickersPerSet+1),
	}
	for name, doc := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseTemplate([]byte(doc)); err == nil {
				t.Fatalf("expected %q to be rejected", name)
			}
		})
	}

	if _, err := ParseTemplate([]byte(valid)); err != nil {
		t.Fatalf("control: valid template rejected: %v", err)
	}
	if _, err := ParseTemplate(make([]byte, MaxUploadBytes+1)); err == nil {
		t.Fatalf("expected an oversize upload to be rejected")
	}
}

func TestParseTemplateAcceptsLimitSizedFile(t *testing.T) {
	const head = "schema_version: 1\nset: Kitsu\nstickers: []\n# "
	doc := head + strings.Repeat("x", MaxUploadBytes-len(head)-1) + "\n"
	if len(doc) != MaxUploadBytes {
		t.Fatalf("test document is %d bytes, want %d", len(doc), MaxUploadBytes)
	}
	if _, err := ParseTemplate([]byte(doc)); err != nil {
		t.Fatalf("a file of exactly the limit must be accepted: %v", err)
	}
}

func TestBuildTemplateRejectsAmbiguousRemote(t *testing.T) {
	if _, err := BuildTemplate(State{}, remoteSet("Kitsu", "u1", "u1")); err == nil {
		t.Fatalf("duplicate file_unique_id from Telegram must be rejected")
	}
	if _, err := BuildTemplate(State{}, remoteSet("bad-name", "u1")); err == nil {
		t.Fatalf("malformed set name must be rejected")
	}
}

// maximalCatalog builds the largest valid catalog of one set: the maximum
// number of available and retained entries, each with a full-length
// description and the maximum number of full-length tags made of fill.
func maximalCatalog(t *testing.T, fill string) (State, RemoteSet) {
	t.Helper()
	remote := RemoteSet{Name: "Kitsu", Title: "Kitsu"}
	var entries []Entry
	for i := 0; i < MaxStickersPerSet+MaxMissingPerSet; i++ {
		tags := make([]string, MaxTags)
		for j := range tags {
			tags[j] = strings.Repeat(fill, MaxTagRunes-2) + fmt.Sprintf("%02d", j)
		}
		unique := fmt.Sprintf("u%04d", i)
		e := annotated(unique, fmt.Sprintf("kitsu_%04d", i), strings.Repeat(fill, MaxDescriptionRunes), tags...)
		e.Available = i < MaxStickersPerSet
		entries = append(entries, e)
		if e.Available {
			remote.Stickers = append(remote.Stickers, RemoteSticker{FileUniqueID: unique, FileID: "file-" + unique, Emoji: "🦊", Format: FormatStatic})
		}
	}

	return stateFrom(t, entries...), remote
}

// A maximally annotated catalog, retained entries included, must export to a
// file the upload path accepts, and the parsed file must carry every
// annotation back unchanged. Characters outside the BMP are the worst case:
// the YAML encoder escapes each as \UXXXXXXXX.
func TestMaximalTemplateRoundTrips(t *testing.T) {
	for name, fill := range map[string]string{
		"ascii": "a", "escapes": `"`, "backslash": `\`, "cjk": "狐", "astral": "😀",
	} {
		t.Run(name, func(t *testing.T) {
			stored, remote := maximalCatalog(t, fill)
			raw, err := BuildTemplate(stored, remote)
			if err != nil {
				t.Fatalf("BuildTemplate: %v", err)
			}
			if len(raw) > MaxUploadBytes {
				t.Fatalf("export is %d bytes, over the %d upload limit", len(raw), MaxUploadBytes)
			}
			tmpl, err := ParseTemplate(raw)
			if err != nil {
				t.Fatalf("an exported template must be uploadable: %v", err)
			}
			if len(tmpl.Stickers) != MaxStickersPerSet || len(tmpl.Missing) != MaxMissingPerSet {
				t.Fatalf("lost entries: %d stickers, %d missing", len(tmpl.Stickers), len(tmpl.Missing))
			}
			byID := map[string]Entry{}
			for _, e := range stored.Entries {
				byID[e.FileUniqueID] = e
			}
			for _, ts := range tmpl.Stickers {
				want := byID[ts.FileUniqueID]
				if ts.ID != want.SemanticID || ts.Description != want.Description || !sameTags(ts.Tags, want.Tags) {
					t.Fatalf("sticker %s lost or altered annotations", ts.FileUniqueID)
				}
			}
			for _, tm := range tmpl.Missing {
				want := byID[tm.FileUniqueID]
				if tm.ID != want.SemanticID || tm.Description != want.Description || !sameTags(tm.Tags, want.Tags) {
					t.Fatalf("missing entry %s lost or altered annotations", tm.FileUniqueID)
				}
			}

			plan := mustPlan(t, stored, tmpl, remote)
			if plan.Report.Unchanged != MaxStickersPerSet || plan.Report.Updated != 0 {
				t.Fatalf("re-importing the export must change nothing: %+v", plan.Report)
			}
		})
	}
}
