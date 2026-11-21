package stickers

import (
	"strings"
	"testing"
	"time"
)

var testNow = time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)

func remoteSet(name string, uniques ...string) RemoteSet {
	rs := RemoteSet{Name: name, Title: strings.ToUpper(name)}
	for _, u := range uniques {
		rs.Stickers = append(rs.Stickers, RemoteSticker{FileUniqueID: u, FileID: "file-" + u, Emoji: "🦊", Format: FormatStatic})
	}

	return rs
}

func boolp(b bool) *bool { return &b }

// stateFrom builds a validated state with one "Kitsu" set holding the given
// entries.
func stateFrom(t *testing.T, entries ...Entry) State {
	const set = "Kitsu"
	t.Helper()
	s := State{Sets: []Set{{Name: set, Title: strings.ToUpper(set), Enabled: true, Revision: 1, SyncedAt: testNow, CreatedAt: testNow, UpdatedAt: testNow}}}
	for i := range entries {
		e := entries[i]
		e.SetName = set
		if e.FileID == "" {
			e.FileID = "file-" + e.FileUniqueID
		}
		if e.Format == "" {
			e.Format = FormatStatic
		}
		if e.Position == 0 {
			e.Position = i + 1
		}
		s.Entries = append(s.Entries, e)
	}
	if err := ValidateState(s); err != nil {
		t.Fatalf("test state invalid: %v", err)
	}

	return s
}

func annotated(unique, id, desc string, tags ...string) Entry {
	return Entry{FileUniqueID: unique, SemanticID: id, Description: desc, Tags: tags, Emoji: "🦊", Available: true, Enabled: true, SyncedAt: testNow, UpdatedAt: testNow}
}

func mustPlan(t *testing.T, stored State, tmpl *Template, remote RemoteSet) *ImportPlan {
	t.Helper()
	// A hand-built template stands for one exported from the current state.
	if tmpl.CatalogVersion == "" {
		tmpl.CatalogVersion = AnnotationVersion(stored, tmpl.Set)
	}
	plan, err := PlanImport(stored, tmpl, remote, testNow.Add(time.Hour))
	if err != nil {
		t.Fatalf("PlanImport: %v", err)
	}

	return plan
}

func entryOf(plan *ImportPlan, unique string) Entry {
	for _, e := range plan.SetEntries {
		if e.FileUniqueID == unique {
			return e
		}
	}

	return Entry{}
}
