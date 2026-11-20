package stickers

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Report counts what one import changed. Each sticker currently in Telegram
// lands in exactly one of Added, Updated, Reset or Unchanged; Renamed is a
// subset of Updated.
type Report struct {
	Added     int // stickers seen for the first time
	Updated   int // existing stickers whose id, description, tags or enabled flag changed
	Renamed   int // subset of Updated: a non-empty semantic id was replaced
	Reset     int // existing stickers whose annotations were cleared by reset
	Unchanged int // existing stickers with no curated change (includes no-op resets)
	// Incomplete counts available stickers that cannot be selected yet
	// because they lack an id or a description.
	Incomplete     int
	NowUnavailable int // stickers that disappeared from Telegram and were retained
	Forgotten      int // retained entries deleted by forget
	// PreservedBlank counts existing stickers where a blank field kept its
	// saved value instead of wiping it.
	PreservedBlank      int
	NotInFile           int // Telegram stickers the file did not list
	ResetNoop           int // reset requested on a sticker with nothing stored
	IgnoredByReset      int // populated id/description/tags overridden by reset
	IgnoredMissingEdits int // edits to missing entries (only forget is editable)
}

// ImportPlan is the validated outcome of merging a template into the saved
// catalog. Nothing is committed yet: the caller writes it in one transaction
// and publishes Next (and an index built from it) only after the commit.
type ImportPlan struct {
	// Set is the prospective set row (Revision is the value after commit).
	Set Set
	// ExpectedRevision is the stored revision the write is guarded by; 0
	// means the set does not exist yet.
	ExpectedRevision int64
	// SetEntries are all final entries of the set: Telegram stickers in
	// Telegram order, then retained unavailable ones.
	SetEntries []Entry
	// Forgotten are file_unique_ids to delete.
	Forgotten []string
	// Release are surviving rows whose stored semantic id changes; the
	// store clears them before the final upserts so a swap or reuse never
	// trips the unique index mid-transaction.
	Release []string
	Next    State
	Report  Report
}

// PlanImport merges an uploaded template into the saved catalog against the
// authoritative current Telegram set. It is pure: it validates the whole
// prospective catalog and returns a *ValidationError (Stale for a template
// that no longer matches Telegram) without side effects.
//
// Merge rules: a blank id/description/tags keeps the saved value; an omitted
// enabled keeps the saved flag; reset: true overrides every editable field of
// its entry; stickers Telegram no longer reports become unavailable but keep
// their annotations unless forget: true deletes them.
func PlanImport(stored State, tmpl *Template, remote RemoteSet, now time.Time) (*ImportPlan, error) {
	if err := ValidateSetName(remote.Name); err != nil {
		return nil, err
	}
	if !strings.EqualFold(tmpl.Set, remote.Name) {
		return nil, fmt.Errorf("the template is for set %q but Telegram returned %q", tmpl.Set, remote.Name)
	}
	if len(remote.Stickers) > MaxStickersPerSet {
		return nil, fmt.Errorf("sticker set has more than %d stickers", MaxStickersPerSet)
	}

	remoteByID := make(map[string]struct{}, len(remote.Stickers))
	for _, rs := range remote.Stickers {
		if rs.FileUniqueID == "" {
			return nil, fmt.Errorf("telegram returned a sticker without file_unique_id")
		}
		if _, dup := remoteByID[rs.FileUniqueID]; dup {
			return nil, fmt.Errorf("telegram returned the same sticker twice; identity is ambiguous")
		}
		remoteByID[rs.FileUniqueID] = struct{}{}
	}

	storedSet, hasSet := stored.Set(remote.Name)
	storedEntries := map[string]Entry{}
	for _, e := range stored.EntriesOf(remote.Name) {
		storedEntries[e.FileUniqueID] = e
	}

	// Stale template: anything that no longer lines up with Telegram is
	// reported precisely and never silently remapped or dropped.
	var stale issueList
	for _, ts := range tmpl.Stickers {
		if _, ok := remoteByID[ts.FileUniqueID]; !ok {
			stale.addf("sticker %s is no longer in the Telegram set", ts.FileUniqueID)
		}
	}
	for _, tm := range tmpl.Missing {
		if _, ok := remoteByID[tm.FileUniqueID]; ok && flag(tm.Forget) {
			stale.addf("sticker %s is back in the Telegram set and cannot be forgotten", tm.FileUniqueID)
		}
	}
	if err := stale.err(true); err != nil {
		return nil, err
	}

	var issues issueList
	templateByID := make(map[string]TemplateSticker, len(tmpl.Stickers))
	for _, ts := range tmpl.Stickers {
		if _, dup := templateByID[ts.FileUniqueID]; dup {
			issues.addf("sticker %s appears twice in the file", ts.FileUniqueID)
		}
		templateByID[ts.FileUniqueID] = ts
	}

	forgotten := map[string]struct{}{}
	var report Report
	seenMissing := map[string]struct{}{}
	for _, tm := range tmpl.Missing {
		if _, dup := seenMissing[tm.FileUniqueID]; dup {
			issues.addf("missing entry %s appears twice in the file", tm.FileUniqueID)
		}
		seenMissing[tm.FileUniqueID] = struct{}{}
		if _, ok := templateByID[tm.FileUniqueID]; ok {
			issues.addf("sticker %s is listed both under stickers and under missing", tm.FileUniqueID)
		}
		saved, known := storedEntries[tm.FileUniqueID]
		if !known {
			issues.addf("missing entry %s is not in the saved catalog", tm.FileUniqueID)

			continue
		}
		if flag(tm.Forget) {
			forgotten[tm.FileUniqueID] = struct{}{}

			continue
		}
		if strings.TrimSpace(tm.ID) != saved.SemanticID || strings.TrimSpace(tm.Description) != saved.Description ||
			!sameTags(normalizeTags(tm.Tags), saved.Tags) {
			report.IgnoredMissingEdits++
		}
	}

	setEntries := make([]Entry, 0, len(remote.Stickers)+len(storedEntries))
	for i, rs := range remote.Stickers {
		saved, hasSaved := storedEntries[rs.FileUniqueID]
		ts, hasTemplate := templateByID[rs.FileUniqueID]

		final := Entry{
			SetName: remote.Name, FileUniqueID: rs.FileUniqueID, FileID: rs.FileID, Emoji: rs.Emoji,
			Format: rs.Format, Position: i + 1, Available: true, Enabled: true, SyncedAt: now, UpdatedAt: now,
		}
		if hasSaved {
			final.SemanticID, final.Description = saved.SemanticID, saved.Description
			final.Tags = append([]string(nil), saved.Tags...)
			final.Enabled = saved.Enabled
		}

		changed, renamed, reset, preserved := false, false, false, false
		switch {
		case hasTemplate && flag(ts.Reset):
			if hasSaved && (saved.Annotated() || !saved.Enabled) {
				final.SemanticID, final.Description, final.Tags, final.Enabled = "", "", nil, true
				reset = true
			} else {
				report.ResetNoop++
			}
			report.IgnoredByReset += populatedFields(ts)
		case hasTemplate:
			id, desc, tags := strings.TrimSpace(ts.ID), strings.TrimSpace(ts.Description), normalizeTags(ts.Tags)

			switch {
			case id != "" && id != final.SemanticID:
				renamed = final.SemanticID != ""
				final.SemanticID, changed = id, true
			case id == "" && final.SemanticID != "":
				preserved = true
			}
			switch {
			case desc != "" && desc != final.Description:
				final.Description, changed = desc, true
			case desc == "" && final.Description != "":
				preserved = true
			}
			switch {
			case len(tags) > 0 && !sameTags(tags, final.Tags):
				final.Tags, changed = tags, true
			case len(tags) == 0 && len(final.Tags) > 0:
				preserved = true
			}
			if ts.Enabled != nil && *ts.Enabled != final.Enabled {
				final.Enabled, changed = *ts.Enabled, true
			}
		default:
			report.NotInFile++
		}

		switch {
		case !hasSaved:
			report.Added++
		case reset:
			report.Reset++
		case changed:
			report.Updated++
			if renamed {
				report.Renamed++
			}
		default:
			report.Unchanged++
		}
		if preserved && hasSaved {
			report.PreservedBlank++
		}
		if hasSaved && !changed && !reset {
			final.UpdatedAt = saved.UpdatedAt
		}
		if !final.Complete() {
			report.Incomplete++
		}
		setEntries = append(setEntries, final)
	}

	var retained []Entry
	for unique, saved := range storedEntries {
		if _, ok := remoteByID[unique]; ok {
			continue
		}
		if _, drop := forgotten[unique]; drop {
			report.Forgotten++

			continue
		}
		keep := saved
		keep.Tags = append([]string(nil), saved.Tags...)
		if saved.Available {
			keep.Available, keep.UpdatedAt = false, now
			report.NowUnavailable++
		}
		retained = append(retained, keep)
	}
	sort.Slice(retained, func(i, j int) bool { return retained[i].FileUniqueID < retained[j].FileUniqueID })
	setEntries = append(setEntries, retained...)

	set := Set{
		Name: remote.Name, Title: SanitizeTitle(remote.Title), Enabled: true,
		SyncedAt: now, CreatedAt: now, UpdatedAt: now,
	}
	var expected int64
	if hasSet {
		set.Enabled, set.CreatedAt, expected = storedSet.Enabled, storedSet.CreatedAt, storedSet.Revision
	}
	set.Revision = expected + 1

	finalByID := make(map[string]Entry, len(setEntries))
	for _, e := range setEntries {
		finalByID[e.FileUniqueID] = e
	}
	var release []string
	for unique, saved := range storedEntries {
		if _, drop := forgotten[unique]; drop || saved.SemanticID == "" {
			continue
		}
		if final, ok := finalByID[unique]; ok && final.SemanticID != saved.SemanticID {
			release = append(release, unique)
		}
	}
	sort.Strings(release)

	forgottenList := make([]string, 0, len(forgotten))
	for unique := range forgotten {
		forgottenList = append(forgottenList, unique)
	}
	sort.Strings(forgottenList)

	next := replaceSet(stored, set, setEntries)
	if err := ValidateState(next); err != nil {
		// Combine the template-level problems found above with the
		// catalog-level ones so the administrator fixes everything at once.
		var verr *ValidationError
		if asValidationError(err, &verr) {
			for _, item := range verr.Issues {
				issues.addf("%s", item)
			}
			issues.total += verr.Total - len(verr.Issues)
		}
	}
	if err := issues.err(false); err != nil {
		return nil, err
	}

	return &ImportPlan{
		Set: set, ExpectedRevision: expected, SetEntries: setEntries, Forgotten: forgottenList,
		Release: release, Next: next, Report: report,
	}, nil
}

func asValidationError(err error, target **ValidationError) bool {
	v, ok := err.(*ValidationError) //nolint:errorlint // ValidateState returns the concrete type directly
	if ok {
		*target = v
	}

	return ok
}

// replaceSet returns stored with one set (and its entries) replaced or added,
// in the same order the store loads them: sets by name, entries by set,
// position, file_unique_id.
func replaceSet(stored State, set Set, entries []Entry) State {
	next := State{Sets: make([]Set, 0, len(stored.Sets)+1), Entries: make([]Entry, 0, len(stored.Entries)+len(entries))}
	for _, s := range stored.Sets {
		if s.Name != set.Name {
			next.Sets = append(next.Sets, s)
		}
	}
	next.Sets = append(next.Sets, set)
	sort.Slice(next.Sets, func(i, j int) bool { return next.Sets[i].Name < next.Sets[j].Name })

	for _, e := range stored.Entries {
		if e.SetName != set.Name {
			next.Entries = append(next.Entries, e)
		}
	}
	next.Entries = append(next.Entries, entries...)
	SortEntries(next.Entries)

	return next
}

// SortEntries orders entries like the store loads them.
func SortEntries(entries []Entry) {
	sort.SliceStable(entries, func(i, j int) bool {
		a, b := entries[i], entries[j]
		if a.SetName != b.SetName {
			return a.SetName < b.SetName
		}
		if a.Position != b.Position {
			return a.Position < b.Position
		}

		return a.FileUniqueID < b.FileUniqueID
	})
}

func populatedFields(ts TemplateSticker) int {
	n := 0
	if strings.TrimSpace(ts.ID) != "" {
		n++
	}
	if strings.TrimSpace(ts.Description) != "" {
		n++
	}
	if len(normalizeTags(ts.Tags)) > 0 {
		n++
	}

	return n
}

// normalizeTags trims each tag. Empty entries are kept so validation can
// report them; a nil or empty list means "blank, keep the saved tags".
func normalizeTags(tags []string) []string {
	if len(tags) == 0 {
		return nil
	}
	out := make([]string, len(tags))
	for i, tag := range tags {
		out[i] = strings.TrimSpace(tag)
	}

	return out
}

func sameTags(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}

	return true
}
