package stickers

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func tmplOf(set string, stickers ...TemplateSticker) *Template {
	return &Template{SchemaVersion: SchemaVersion, Set: set, Stickers: stickers}
}

func baseCatalog(t *testing.T) State {
	t.Helper()

	return stateFrom(t,
		annotated("a", "id_a", "Fox A", "tag_a"),
		annotated("b", "id_b", "Fox B", "tag_b"),
		Entry{FileUniqueID: "c", Available: true, Enabled: true},
	)
}

func TestImportBlankFieldsKeepStoredValuesAndOmittedEnabledKeepsFlag(t *testing.T) {
	stored := baseCatalog(t)
	stored.Entries[1].Enabled = false // b is saved as disabled

	// Both entries come back blank, with enabled omitted.
	plan := mustPlan(t, stored, tmplOf("Kitsu",
		TemplateSticker{FileUniqueID: "a"}, TemplateSticker{FileUniqueID: "b"}, TemplateSticker{FileUniqueID: "c"}),
		remoteSet("Kitsu", "a", "b", "c"))

	if a := entryOf(plan, "a"); a.SemanticID != "id_a" || a.Description != "Fox A" || len(a.Tags) != 1 || !a.Enabled {
		t.Fatalf("blank fields must keep the saved annotation: %+v", a)
	}
	if b := entryOf(plan, "b"); b.Enabled || b.SemanticID != "id_b" {
		t.Fatalf("an omitted enabled must keep the saved false: %+v", b)
	}
	if r := plan.Report; r.Unchanged != 3 || r.PreservedBlank != 2 || r.Updated != 0 {
		t.Fatalf("unexpected report: %+v", r)
	}
}

func TestImportExplicitEnabledFalseDisablesAndTrueEnables(t *testing.T) {
	stored := baseCatalog(t)
	stored.Entries[1].Enabled = false

	plan := mustPlan(t, stored, tmplOf("Kitsu",
		TemplateSticker{FileUniqueID: "a", Enabled: boolp(false)}, TemplateSticker{FileUniqueID: "b", Enabled: boolp(true)}),
		remoteSet("Kitsu", "a", "b", "c"))

	if entryOf(plan, "a").Enabled || !entryOf(plan, "b").Enabled {
		t.Fatalf("explicit enabled values must apply")
	}
	if plan.Report.Updated != 2 {
		t.Fatalf("expected two updates, got %+v", plan.Report)
	}
}

func TestImportRenameAndUpdateAreReported(t *testing.T) {
	plan := mustPlan(t, baseCatalog(t), tmplOf("Kitsu",
		TemplateSticker{FileUniqueID: "a", ID: "id_a2"},
		TemplateSticker{FileUniqueID: "b", Description: "  New B  ", Tags: []string{" t1 ", "t2"}},
		TemplateSticker{FileUniqueID: "c", ID: "id_c", Description: "Fox C"},
	), remoteSet("Kitsu", "a", "b", "c"))

	if a := entryOf(plan, "a"); a.SemanticID != "id_a2" || a.Description != "Fox A" {
		t.Fatalf("rename must keep the description: %+v", a)
	}
	if b := entryOf(plan, "b"); b.Description != "New B" || strings.Join(b.Tags, ",") != "t1,t2" {
		t.Fatalf("values must be trimmed: %+v", b)
	}
	if r := plan.Report; r.Updated != 3 || r.Renamed != 1 || r.Incomplete != 0 {
		t.Fatalf("unexpected report: %+v", r)
	}
	if got := strings.Join(plan.Release, ","); got != "a" {
		t.Fatalf("only the renamed row releases its old id, got %q", got)
	}
}

func TestImportResetWorksDirectlyOnAPopulatedRepeatExportTemplate(t *testing.T) {
	stored := baseCatalog(t)
	stored.Entries[0].Enabled = false
	remote := remoteSet("Kitsu", "a", "b", "c")

	raw, err := BuildTemplate(stored, remote)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	tmpl, err := ParseTemplate(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	// Flip only the single flag on a fully populated entry.
	if tmpl.Stickers[0].ID == "" || tmpl.Stickers[0].Enabled == nil || *tmpl.Stickers[0].Enabled {
		t.Fatalf("precondition: entry a must come back populated and disabled: %+v", tmpl.Stickers[0])
	}
	tmpl.Stickers[0].Reset = boolp(true)

	plan := mustPlan(t, stored, tmpl, remote)
	a := entryOf(plan, "a")
	if a.SemanticID != "" || a.Description != "" || len(a.Tags) != 0 || !a.Enabled || a.Active(true) {
		t.Fatalf("reset must clear the entry and restore the default: %+v", a)
	}
	if !a.Available || a.FileID != "file-a" {
		t.Fatalf("reset keeps the Telegram sticker row: %+v", a)
	}
	if r := plan.Report; r.Reset != 1 || r.IgnoredByReset != 3 || r.Updated != 0 || r.Unchanged != 2 {
		t.Fatalf("unexpected report: %+v", r)
	}
	if got := strings.Join(plan.Release, ","); got != "a" {
		t.Fatalf("reset must release the id, got %q", got)
	}

	again, err := BuildTemplate(plan.Next, remote)
	if err != nil {
		t.Fatalf("re-export: %v", err)
	}
	reparsed, _ := ParseTemplate(again)
	if s := reparsed.Stickers[0]; s.ID != "" || s.Description != "" || len(s.Tags) != 0 || !flag(s.Enabled) {
		t.Fatalf("re-export must show the entry blank: %+v", s)
	}
}

func TestImportResetIgnoresInvalidIgnoredValuesAndNoopsWhenNothingStored(t *testing.T) {
	plan := mustPlan(t, baseCatalog(t), tmplOf("Kitsu",
		TemplateSticker{FileUniqueID: "a", Reset: boolp(true), ID: "NOT A VALID ID!!", Tags: []string{"x", "x"}},
		TemplateSticker{FileUniqueID: "c", Reset: boolp(true)},
	), remoteSet("Kitsu", "a", "b", "c"))

	if r := plan.Report; r.Reset != 1 || r.ResetNoop != 1 || r.IgnoredByReset != 2 {
		t.Fatalf("unexpected report: %+v", r)
	}
}

func TestImportResetReleasesIDForReuseInTheSameImport(t *testing.T) {
	plan := mustPlan(t, baseCatalog(t), tmplOf("Kitsu",
		TemplateSticker{FileUniqueID: "a", Reset: boolp(true)},
		TemplateSticker{FileUniqueID: "c", ID: "id_a", Description: "Fox C"},
	), remoteSet("Kitsu", "a", "b", "c"))

	if entryOf(plan, "a").SemanticID != "" || entryOf(plan, "c").SemanticID != "id_a" {
		t.Fatalf("id must move from a to c")
	}
	if got := strings.Join(plan.Release, ","); got != "a" {
		t.Fatalf("expected release of a, got %q", got)
	}
}

func TestImportSwapsIDsBetweenStickers(t *testing.T) {
	plan := mustPlan(t, baseCatalog(t), tmplOf("Kitsu",
		TemplateSticker{FileUniqueID: "a", ID: "id_b"}, TemplateSticker{FileUniqueID: "b", ID: "id_a"},
	), remoteSet("Kitsu", "a", "b", "c"))

	if entryOf(plan, "a").SemanticID != "id_b" || entryOf(plan, "b").SemanticID != "id_a" {
		t.Fatalf("ids must be swapped")
	}
	if got := strings.Join(plan.Release, ","); got != "a,b" {
		t.Fatalf("both rows must release, got %q", got)
	}
}

func TestImportForgetFreesIDAndDeletesRetainedEntry(t *testing.T) {
	stored := stateFrom(t,
		annotated("a", "id_a", "Fox A"),
		Entry{FileUniqueID: "gone", SemanticID: "id_gone", Description: "Gone", Available: false, Enabled: true},
		Entry{FileUniqueID: "keep", SemanticID: "id_keep", Description: "Keep", Available: false, Enabled: true},
	)

	plan := mustPlan(t, stored, &Template{SchemaVersion: 1, Set: "Kitsu",
		Stickers: []TemplateSticker{{FileUniqueID: "a", ID: "id_gone"}},
		Missing: []TemplateMissing{
			{FileUniqueID: "gone", Forget: boolp(true)},
			{FileUniqueID: "keep", ID: "edited_ignored", Forget: boolp(false)},
		}}, remoteSet("Kitsu", "a"))

	if got := strings.Join(plan.Forgotten, ","); got != "gone" || plan.Report.Forgotten != 1 {
		t.Fatalf("expected gone forgotten, got %q %+v", got, plan.Report)
	}
	if entryOf(plan, "gone").FileUniqueID != "" || entryOf(plan, "keep").SemanticID != "id_keep" {
		t.Fatalf("forgotten rows are deleted, others retained: %+v", plan.SetEntries)
	}
	if entryOf(plan, "a").SemanticID != "id_gone" {
		t.Fatalf("the forgotten id must be reusable")
	}
	if plan.Report.IgnoredMissingEdits != 1 {
		t.Fatalf("edits to a missing entry are ignored and counted: %+v", plan.Report)
	}
}

func TestImportMarksVanishedStickersUnavailableButKeepsAnnotations(t *testing.T) {
	plan := mustPlan(t, baseCatalog(t), tmplOf("Kitsu", TemplateSticker{FileUniqueID: "a"}), remoteSet("Kitsu", "a", "newone"))

	b := entryOf(plan, "b")
	if b.Available || b.SemanticID != "id_b" || b.Description != "Fox B" {
		t.Fatalf("b must be retained as unavailable with its annotation: %+v", b)
	}
	if plan.Next.Entries == nil || BuildIndex(plan.Next).Len() != 1 {
		t.Fatalf("only a is active (b unavailable, c blank, newone blank), got %d", BuildIndex(plan.Next).Len())
	}
	if r := plan.Report; r.NowUnavailable != 2 || r.Added != 1 || r.NotInFile != 1 || r.Incomplete != 1 {
		t.Fatalf("unexpected report: %+v", r)
	}
}

func TestImportReorderOnlyChangesNothing(t *testing.T) {
	stored := baseCatalog(t)
	plan := mustPlan(t, stored, tmplOf("Kitsu",
		TemplateSticker{FileUniqueID: "c"}, TemplateSticker{FileUniqueID: "b"}, TemplateSticker{FileUniqueID: "a"}),
		remoteSet("Kitsu", "c", "b", "a"))

	if r := plan.Report; r.Unchanged != 3 || r.Updated+r.Added+r.Reset != 0 {
		t.Fatalf("a pure reorder must not change curated data: %+v", r)
	}
	if entryOf(plan, "a").Position != 3 || entryOf(plan, "c").Position != 1 {
		t.Fatalf("positions follow Telegram's new order")
	}
	if len(plan.Release) != 0 {
		t.Fatalf("a reorder releases nothing: %v", plan.Release)
	}
}

func TestImportRejectsStaleTemplates(t *testing.T) {
	stored := stateFrom(t,
		annotated("a", "id_a", "Fox A"),
		Entry{FileUniqueID: "back", SemanticID: "id_back", Description: "Back", Available: false, Enabled: true},
	)

	cases := map[string]*Template{
		"replaced media": tmplOf("Kitsu", TemplateSticker{FileUniqueID: "a"}, TemplateSticker{FileUniqueID: "old-media"}),
		"forget reappeared": {SchemaVersion: 1, Set: "Kitsu", Stickers: []TemplateSticker{{FileUniqueID: "a"}},
			Missing: []TemplateMissing{{FileUniqueID: "back", Forget: boolp(true)}}},
	}
	for name, tmpl := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := PlanImport(stored, tmpl, remoteSet("Kitsu", "a", "back"), testNow)
			var verr *ValidationError
			if !errors.As(err, &verr) || !verr.Stale || verr.Total != 1 {
				t.Fatalf("expected a stale ValidationError, got %v", err)
			}
		})
	}
}

func TestImportRejectsInvalidInput(t *testing.T) {
	other := State{
		Sets:    []Set{{Name: "Kitsu", Title: "K", Enabled: true, Revision: 1}, {Name: "Other", Title: "O", Enabled: true, Revision: 1}},
		Entries: []Entry{{SetName: "Other", FileUniqueID: "o1", FileID: "f", SemanticID: "taken_id", Description: "x", Format: FormatStatic, Position: 1, Available: true, Enabled: true}},
	}

	cases := map[string]struct {
		state State
		tmpl  *Template
	}{
		"malformed id":         {State{}, tmplOf("Kitsu", TemplateSticker{FileUniqueID: "a", ID: "Bad-ID", Description: "d"})},
		"too short id":         {State{}, tmplOf("Kitsu", TemplateSticker{FileUniqueID: "a", ID: "ab", Description: "d"})},
		"duplicate id in file": {State{}, tmplOf("Kitsu", TemplateSticker{FileUniqueID: "a", ID: "same_id", Description: "d"}, TemplateSticker{FileUniqueID: "b", ID: "same_id", Description: "d"})},
		"collides across sets": {other, tmplOf("Kitsu", TemplateSticker{FileUniqueID: "a", ID: "taken_id", Description: "d"})},
		"multiline desc":       {State{}, tmplOf("Kitsu", TemplateSticker{FileUniqueID: "a", ID: "good_id", Description: "line1\nline2"})},
		"long desc":            {State{}, tmplOf("Kitsu", TemplateSticker{FileUniqueID: "a", ID: "good_id", Description: strings.Repeat("я", MaxDescriptionRunes+1)})},
		"too many tags":        {State{}, tmplOf("Kitsu", TemplateSticker{FileUniqueID: "a", ID: "good_id", Description: "d", Tags: manyTags(MaxTags + 1)})},
		"empty tag":            {State{}, tmplOf("Kitsu", TemplateSticker{FileUniqueID: "a", ID: "good_id", Description: "d", Tags: []string{"ok", "  "}})},
		"duplicate tag":        {State{}, tmplOf("Kitsu", TemplateSticker{FileUniqueID: "a", ID: "good_id", Description: "d", Tags: []string{"Fox", "fox"}})},
		"duplicate sticker":    {State{}, tmplOf("Kitsu", TemplateSticker{FileUniqueID: "a"}, TemplateSticker{FileUniqueID: "a"})},
		"unknown missing": {State{}, &Template{SchemaVersion: 1, Set: "Kitsu",
			Missing: []TemplateMissing{{FileUniqueID: "nope", Forget: boolp(true)}}}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := PlanImport(tc.state, tc.tmpl, remoteSet("Kitsu", "a", "b"), testNow)
			var verr *ValidationError
			if !errors.As(err, &verr) || verr.Stale || verr.Total == 0 {
				t.Fatalf("expected a validation error, got %v", err)
			}
		})
	}
}

func manyTags(n int) []string {
	tags := make([]string, n)
	for i := range tags {
		tags[i] = fmt.Sprintf("tag%d", i)
	}

	return tags
}

func TestImportAtTheLimitsAndBeyond(t *testing.T) {
	maxID := "a" + strings.Repeat("b", 47)
	ok := tmplOf("Kitsu", TemplateSticker{
		FileUniqueID: "a", ID: maxID, Description: strings.Repeat("я", MaxDescriptionRunes),
		Tags: append(manyTags(MaxTags-1), strings.Repeat("т", MaxTagRunes)),
	})
	if _, err := PlanImport(State{}, ok, remoteSet("Kitsu", "a"), testNow); err != nil {
		t.Fatalf("values exactly at the limits must be accepted: %v", err)
	}

	for name, tmpl := range map[string]*Template{
		"id too long":  tmplOf("Kitsu", TemplateSticker{FileUniqueID: "a", ID: maxID + "c", Description: "d"}),
		"tag too long": tmplOf("Kitsu", TemplateSticker{FileUniqueID: "a", ID: maxID, Description: "d", Tags: []string{strings.Repeat("т", MaxTagRunes+1)}}),
	} {
		if _, err := PlanImport(State{}, tmpl, remoteSet("Kitsu", "a"), testNow); err == nil {
			t.Fatalf("%s must be rejected", name)
		}
	}

	// Telegram-side cap on set size.
	uniques := make([]string, MaxStickersPerSet+1)
	for i := range uniques {
		uniques[i] = fmt.Sprintf("u%d", i)
	}
	if _, err := PlanImport(State{}, tmplOf("Kitsu"), remoteSet("Kitsu", uniques...), testNow); err == nil {
		t.Fatalf("a set above the per-set cap must be rejected")
	}
	if _, err := PlanImport(State{}, tmplOf("Kitsu"), remoteSet("Kitsu", uniques[:MaxStickersPerSet]...), testNow); err != nil {
		t.Fatalf("a set exactly at the cap must be accepted: %v", err)
	}
}

func TestImportEnforcesCatalogWideCaps(t *testing.T) {
	big := State{}
	for i := 0; i < MaxSets; i++ {
		big.Sets = append(big.Sets, Set{Name: fmt.Sprintf("S%d", i), Title: "t", Enabled: true, Revision: 1})
	}
	if _, err := PlanImport(big, tmplOf("Extra"), remoteSet("Extra", "a"), testNow); err == nil {
		t.Fatalf("a %dth set must be rejected", MaxSets+1)
	}
}

func TestImportRejectsMismatchedSetAndAmbiguousRemote(t *testing.T) {
	if _, err := PlanImport(State{}, tmplOf("Other"), remoteSet("Kitsu", "a"), testNow); err == nil {
		t.Fatalf("a template for a different set must be rejected")
	}
	if _, err := PlanImport(State{}, tmplOf("kitsu"), remoteSet("Kitsu", "a"), testNow); err != nil {
		t.Fatalf("set names compare case-insensitively against Telegram's canonical name: %v", err)
	}
	if _, err := PlanImport(State{}, tmplOf("Kitsu"), remoteSet("Kitsu", "a", "a"), testNow); err == nil {
		t.Fatalf("duplicate file_unique_id from Telegram must be rejected")
	}
}

func TestImportValidationErrorsAreBoundedAndNeverEchoAnnotationText(t *testing.T) {
	var stickers []TemplateSticker
	uniques := make([]string, 30)
	for i := range uniques {
		uniques[i] = fmt.Sprintf("u%d", i)
		stickers = append(stickers, TemplateSticker{FileUniqueID: uniques[i], ID: "BAD", Description: "SECRET-DESCRIPTION"})
	}
	_, err := PlanImport(State{}, tmplOf("Kitsu", stickers...), remoteSet("Kitsu", uniques...), testNow)
	var verr *ValidationError
	if !errors.As(err, &verr) {
		t.Fatalf("expected a validation error, got %v", err)
	}
	if len(verr.Issues) != MaxIssues || verr.Total <= MaxIssues {
		t.Fatalf("issues must be capped at %d with the total counted: %d/%d", MaxIssues, len(verr.Issues), verr.Total)
	}
	if strings.Contains(err.Error(), "SECRET-DESCRIPTION") {
		t.Fatalf("errors must not echo annotation text: %v", err)
	}
}

func TestPlanImportDoesNotMutateTheStoredState(t *testing.T) {
	stored := baseCatalog(t)
	snapshot := fmt.Sprint(stored)
	_ = mustPlan(t, stored, tmplOf("Kitsu", TemplateSticker{FileUniqueID: "a", ID: "id_other"}, TemplateSticker{FileUniqueID: "b", Reset: boolp(true)}), remoteSet("Kitsu", "a", "b"))
	if fmt.Sprint(stored) != snapshot {
		t.Fatalf("PlanImport must be pure")
	}
}

func TestFirstImportExpectsNoRevisionAndLaterImportsBumpIt(t *testing.T) {
	first := mustPlan(t, State{}, tmplOf("Kitsu"), remoteSet("Kitsu", "a"))
	if first.ExpectedRevision != 0 || first.Set.Revision != 1 || !first.Set.Enabled {
		t.Fatalf("first import: %+v", first.Set)
	}
	second := mustPlan(t, first.Next, tmplOf("Kitsu"), remoteSet("Kitsu", "a"))
	if second.ExpectedRevision != 1 || second.Set.Revision != 2 {
		t.Fatalf("second import must expect the stored revision: %+v / %d", second.Set, second.ExpectedRevision)
	}
}

// exportTemplate renders and parses a real export of the given catalog.
func exportTemplate(t *testing.T, stored State, remote RemoteSet) *Template {
	t.Helper()
	raw, err := BuildTemplate(stored, remote)
	if err != nil {
		t.Fatalf("BuildTemplate: %v", err)
	}
	tmpl, err := ParseTemplate(raw)
	if err != nil {
		t.Fatalf("ParseTemplate: %v", err)
	}

	return tmpl
}

func expectStale(t *testing.T, err error) {
	t.Helper()
	var verr *ValidationError
	if !errors.As(err, &verr) || !verr.Stale {
		t.Fatalf("expected a stale-template error, got %v", err)
	}
}

func TestStaleTemplateCannotOverwriteLaterAnnotationChanges(t *testing.T) {
	stored := baseCatalog(t)
	remote := remoteSet("Kitsu", "a", "b", "c")

	// Two curators export the same catalog.
	first := exportTemplate(t, stored, remote)
	second := exportTemplate(t, stored, remote)

	first.Stickers[0].Description = "Fox A, edited by the first curator"
	plan, err := PlanImport(stored, first, remote, testNow)
	if err != nil {
		t.Fatalf("the first import must succeed: %v", err)
	}

	// The second export predates that change: its conflicting edit is refused,
	// whether it touches the same sticker or a different one.
	second.Stickers[0].Description = "Fox A, edited by the second curator"
	_, err = PlanImport(plan.Next, second, remote, testNow)
	expectStale(t, err)

	other := exportTemplate(t, stored, remote)
	other.Stickers[2].ID, other.Stickers[2].Description = "id_c", "Fox C"
	_, err = PlanImport(plan.Next, other, remote, testNow)
	expectStale(t, err)

	reset := exportTemplate(t, stored, remote)
	reset.Stickers[1].Reset = boolp(true)
	_, err = PlanImport(plan.Next, reset, remote, testNow)
	expectStale(t, err)

	// A fresh export of the current catalog works again.
	fresh := exportTemplate(t, plan.Next, remote)
	fresh.Stickers[0].Description = "Fox A, third edit"
	if _, err := PlanImport(plan.Next, fresh, remote, testNow); err != nil {
		t.Fatalf("a fresh export must be accepted: %v", err)
	}
}

func TestStaleTemplateForgetIsRefused(t *testing.T) {
	stored := stateFrom(t, annotated("a", "id_a", "Fox A"),
		Entry{FileUniqueID: "gone", SemanticID: "id_gone", Description: "Gone", Available: false, Enabled: true})
	remote := remoteSet("Kitsu", "a")

	stale := exportTemplate(t, stored, remote)
	edited := exportTemplate(t, stored, remote)
	edited.Stickers[0].Description = "Fox A, edited"
	plan := mustPlan(t, stored, edited, remote)

	stale.Missing[0].Forget = boolp(true)
	_, err := PlanImport(plan.Next, stale, remote, testNow)
	expectStale(t, err)
}

func TestStaleTemplateThatChangesNothingStillImports(t *testing.T) {
	stored := baseCatalog(t)
	remote := remoteSet("Kitsu", "a", "b", "c")

	tmpl := exportTemplate(t, stored, remote)
	tmpl.Stickers[0].Description = "Fox A, edited"
	plan, err := PlanImport(stored, tmpl, remote, testNow)
	if err != nil {
		t.Fatalf("first import: %v", err)
	}

	// Re-importing the very same file after it was applied is idempotent even
	// though the catalog moved on: it carries no change.
	again, err := PlanImport(plan.Next, tmpl, remote, testNow)
	if err != nil {
		t.Fatalf("an idempotent re-import must not be refused: %v", err)
	}
	if again.Report.Updated != 0 || again.Report.Reset != 0 || again.Report.Unchanged != 3 {
		t.Fatalf("expected no changes, got %+v", again.Report)
	}

	// An old export that only restates saved values or leaves fields blank
	// is equally harmless.
	old := exportTemplate(t, stored, remote)
	old.Stickers[1].Description, old.Stickers[1].ID = "", ""
	old.Stickers[0].Description = "Fox A, edited" // same as now saved
	if _, err := PlanImport(plan.Next, old, remote, testNow); err != nil {
		t.Fatalf("a no-change stale file must be accepted: %v", err)
	}
}

func TestAnnotationVersionIgnoresTelegramDerivedFields(t *testing.T) {
	stored := baseCatalog(t)
	version := AnnotationVersion(stored, "Kitsu")

	moved := stateFrom(t,
		annotated("a", "id_a", "Fox A", "tag_a"),
		annotated("b", "id_b", "Fox B", "tag_b"),
		Entry{FileUniqueID: "c", Available: true, Enabled: true},
		Entry{FileUniqueID: "d", Available: true, Enabled: true}, // new blank sticker
	)
	moved.Entries[0].FileID, moved.Entries[0].Position = "rotated-file-id", 9
	if got := AnnotationVersion(moved, "Kitsu"); got != version {
		t.Fatalf("file_id, position and blank rows must not change the version")
	}

	moved.Entries[1].Description = "Fox B!"
	if got := AnnotationVersion(moved, "Kitsu"); got == version {
		t.Fatalf("an annotation edit must change the version")
	}
}
