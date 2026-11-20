package sqlite

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

var stickerTestTime = time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)

func stickerSet(name string) StickerSetRow {
	return StickerSetRow{Name: name, Title: strings.ToUpper(name), Enabled: true,
		SyncedAt: stickerTestTime, CreatedAt: stickerTestTime, UpdatedAt: stickerTestTime}
}

func stickerEntry(set, unique, semantic string) StickerEntryRow {
	description := ""
	if semantic != "" {
		description = "desc " + semantic
	}

	return StickerEntryRow{SetName: set, FileUniqueID: unique, FileID: "file-" + unique, SemanticID: semantic,
		Description: description, Tags: []string{}, Emoji: "🦊", Format: "static", Position: 1,
		Available: true, Enabled: true, SyncedAt: stickerTestTime, UpdatedAt: stickerTestTime}
}

func unannotatedEntries(set string, n int) []StickerEntryRow {
	entries := make([]StickerEntryRow, 0, n)
	for i := 1; i <= n; i++ {
		e := stickerEntry(set, fmt.Sprintf("%s-u%d", set, i), "")
		e.Position = i
		entries = append(entries, e)
	}

	return entries
}

func mustReplace(t *testing.T, store *Store, w StickerSetWrite) {
	t.Helper()
	if err := store.ReplaceStickerSet(context.Background(), w); err != nil {
		t.Fatalf("ReplaceStickerSet: %v", err)
	}
}

// semanticIDs maps file_unique_id -> semantic id ("" for NULL) for the
// "alpha" set.
func semanticIDs(t *testing.T, store *Store) map[string]string {
	t.Helper()
	_, entries, err := store.LoadStickerCatalog(context.Background())
	if err != nil {
		t.Fatalf("LoadStickerCatalog: %v", err)
	}
	out := map[string]string{}
	for _, e := range entries {
		if e.SetName == "alpha" {
			out[e.FileUniqueID] = e.SemanticID
		}
	}

	return out
}

func setRevision(t *testing.T, store *Store, name string) int64 {
	t.Helper()
	sets, _, err := store.LoadStickerCatalog(context.Background())
	if err != nil {
		t.Fatalf("LoadStickerCatalog: %v", err)
	}
	for _, s := range sets {
		if s.Name == name {
			return s.Revision
		}
	}
	t.Fatalf("set %q not found", name)

	return 0
}

func countWhere(t *testing.T, store *Store, where string) int {
	t.Helper()
	var n int
	if err := store.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM sticker_entries WHERE `+where).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", where, err)
	}

	return n
}

func TestStickerUnannotatedRowsAreSQLNullAndNeverCollide(t *testing.T) {
	store := openTestStore(t)

	mustReplace(t, store, StickerSetWrite{Set: stickerSet("alpha"), Entries: unannotatedEntries("alpha", 5)})
	mustReplace(t, store, StickerSetWrite{Set: stickerSet("beta"), Entries: unannotatedEntries("beta", 3)})

	if got := countWhere(t, store, `semantic_id IS NULL`); got != 8 {
		t.Fatalf("expected 8 NULL semantic ids, got %d", got)
	}
	if got := countWhere(t, store, `semantic_id = ''`); got != 0 {
		t.Fatalf("expected no empty-string semantic ids, got %d", got)
	}

	// Re-import, then annotate one sticker and reset another: both succeed.
	entries := unannotatedEntries("alpha", 5)
	entries[0] = stickerEntry("alpha", "alpha-u1", "kitsu_happy")
	entries[0].Position = 1
	mustReplace(t, store, StickerSetWrite{Set: stickerSet("alpha"), ExpectedRevision: 1, Entries: entries})

	entries[0].SemanticID, entries[0].Description = "", ""
	entries[1] = stickerEntry("alpha", "alpha-u2", "kitsu_sad")
	entries[1].Position = 2
	mustReplace(t, store, StickerSetWrite{Set: stickerSet("alpha"), ExpectedRevision: 2, Entries: entries, Release: []string{"alpha-u1"}})

	ids := semanticIDs(t, store)
	if ids["alpha-u1"] != "" || ids["alpha-u2"] != "kitsu_sad" {
		t.Fatalf("unexpected ids after reset+annotate: %v", ids)
	}
	if got := countWhere(t, store, `semantic_id = ''`); got != 0 {
		t.Fatalf("expected no empty-string semantic ids after rewrites, got %d", got)
	}

	// A direct write of an empty string is rejected by the CHECK constraint.
	if _, err := store.db.ExecContext(context.Background(), `UPDATE sticker_entries SET semantic_id = '' WHERE file_unique_id = 'alpha-u3'`); err == nil {
		t.Fatalf("expected a direct empty semantic_id write to be rejected")
	}
}

func TestStickerRevisionGuard(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)

	mustReplace(t, store, StickerSetWrite{Set: stickerSet("alpha"), Entries: unannotatedEntries("alpha", 1)})
	if rev := setRevision(t, store, "alpha"); rev != 1 {
		t.Fatalf("expected revision 1 after create, got %d", rev)
	}

	// A first-import write over an existing set conflicts.
	err := store.ReplaceStickerSet(ctx, StickerSetWrite{Set: stickerSet("alpha"), Entries: unannotatedEntries("alpha", 2)})
	if !errors.Is(err, ErrStickerSetConflict) {
		t.Fatalf("expected conflict on duplicate create, got %v", err)
	}
	// A stale revision conflicts and changes nothing.
	err = store.ReplaceStickerSet(ctx, StickerSetWrite{Set: stickerSet("alpha"), ExpectedRevision: 7, Entries: unannotatedEntries("alpha", 3)})
	if !errors.Is(err, ErrStickerSetConflict) {
		t.Fatalf("expected conflict on stale revision, got %v", err)
	}
	if got := len(semanticIDs(t, store)); got != 1 {
		t.Fatalf("conflicting writes must not change entries, have %d", got)
	}
	if rev := setRevision(t, store, "alpha"); rev != 1 {
		t.Fatalf("conflicting writes must not bump the revision, got %d", rev)
	}

	// Enabled flag and delete are guarded too.
	if err := store.SetStickerSetEnabled(ctx, "alpha", false, 9, stickerTestTime); !errors.Is(err, ErrStickerSetConflict) {
		t.Fatalf("expected conflict on stale enable, got %v", err)
	}
	if err := store.SetStickerSetEnabled(ctx, "alpha", false, 1, stickerTestTime); err != nil {
		t.Fatalf("SetStickerSetEnabled: %v", err)
	}
	if err := store.DeleteStickerSet(ctx, "alpha", 1); !errors.Is(err, ErrStickerSetConflict) {
		t.Fatalf("expected conflict on stale delete, got %v", err)
	}
	if err := store.DeleteStickerSet(ctx, "alpha", 2); err != nil {
		t.Fatalf("DeleteStickerSet: %v", err)
	}
	if got := countWhere(t, store, `1 = 1`); got != 0 {
		t.Fatalf("expected entries to cascade on set delete, got %d", got)
	}
}

func seedTwoAnnotated(t *testing.T, store *Store) {
	t.Helper()
	a, b := stickerEntry("alpha", "a", "id_a"), stickerEntry("alpha", "b", "id_b")
	a.Position, b.Position = 1, 2
	mustReplace(t, store, StickerSetWrite{Set: stickerSet("alpha"), Entries: []StickerEntryRow{a, b}})
}

func TestStickerResetReleasesIDForReuseInSameWrite(t *testing.T) {
	store := openTestStore(t)
	seedTwoAnnotated(t, store)

	a := stickerEntry("alpha", "a", "")
	b := stickerEntry("alpha", "b", "id_a") // takes the ID A releases
	a.Position, b.Position = 1, 2
	mustReplace(t, store, StickerSetWrite{Set: stickerSet("alpha"), ExpectedRevision: 1,
		Entries: []StickerEntryRow{a, b}, Release: []string{"a", "b"}})

	ids := semanticIDs(t, store)
	if ids["a"] != "" || ids["b"] != "id_a" {
		t.Fatalf("unexpected ids: %v", ids)
	}
}

func TestStickerSwapIDsInOneWrite(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	seedTwoAnnotated(t, store)

	a := stickerEntry("alpha", "a", "id_b")
	b := stickerEntry("alpha", "b", "id_a")
	a.Position, b.Position = 1, 2

	// Without the release phase the sequential upserts collide: this is why
	// the phases exist.
	err := store.ReplaceStickerSet(ctx, StickerSetWrite{Set: stickerSet("alpha"), ExpectedRevision: 1, Entries: []StickerEntryRow{a, b}})
	if err == nil || errors.Is(err, ErrStickerSetConflict) {
		t.Fatalf("expected a unique violation without the release phase, got %v", err)
	}
	if ids := semanticIDs(t, store); ids["a"] != "id_a" || ids["b"] != "id_b" || setRevision(t, store, "alpha") != 1 {
		t.Fatalf("a failed write must leave the catalog untouched: %v", ids)
	}

	mustReplace(t, store, StickerSetWrite{Set: stickerSet("alpha"), ExpectedRevision: 1,
		Entries: []StickerEntryRow{a, b}, Release: []string{"a", "b"}})
	if ids := semanticIDs(t, store); ids["a"] != "id_b" || ids["b"] != "id_a" {
		t.Fatalf("expected swapped ids, got %v", ids)
	}
}

func TestStickerForgottenIDCanBeReused(t *testing.T) {
	store := openTestStore(t)
	seedTwoAnnotated(t, store)

	// Sticker "b" goes missing (kept unavailable), then is forgotten while
	// "a" takes over its ID in the same write.
	b := stickerEntry("alpha", "b", "id_b")
	b.Available, b.Position = false, 2
	mustReplace(t, store, StickerSetWrite{Set: stickerSet("alpha"), ExpectedRevision: 1,
		Entries: []StickerEntryRow{stickerEntry("alpha", "a", "id_a"), b}})

	a := stickerEntry("alpha", "a", "id_b")
	mustReplace(t, store, StickerSetWrite{Set: stickerSet("alpha"), ExpectedRevision: 2,
		Entries: []StickerEntryRow{a}, Forgotten: []string{"b"}, Release: []string{"a"}})

	ids := semanticIDs(t, store)
	if len(ids) != 1 || ids["a"] != "id_b" {
		t.Fatalf("expected only a holding id_b, got %v", ids)
	}
}

func TestStickerFailedReassignmentRollsBackEverything(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	seedTwoAnnotated(t, store)

	// A row in ANOTHER set holds "id_other": taking it fails at the final
	// upsert, after the forget and release phases already ran.
	other := stickerEntry("beta", "o", "id_other")
	mustReplace(t, store, StickerSetWrite{Set: stickerSet("beta"), Entries: []StickerEntryRow{other}})

	before, beforeEntries, err := store.LoadStickerCatalog(ctx)
	if err != nil {
		t.Fatalf("load before: %v", err)
	}

	a := stickerEntry("alpha", "a", "id_other") // collides with beta/o
	err = store.ReplaceStickerSet(ctx, StickerSetWrite{Set: stickerSet("alpha"), ExpectedRevision: 1,
		Entries: []StickerEntryRow{a}, Forgotten: []string{"b"}, Release: []string{"a"}})
	if err == nil {
		t.Fatalf("expected the colliding reassignment to fail")
	}

	after, afterEntries, err := store.LoadStickerCatalog(ctx)
	if err != nil {
		t.Fatalf("load after: %v", err)
	}
	if fmt.Sprint(before) != fmt.Sprint(after) {
		t.Fatalf("set rows changed after rollback:\nbefore %v\nafter  %v", before, after)
	}
	if fmt.Sprint(beforeEntries) != fmt.Sprint(afterEntries) {
		t.Fatalf("entries changed after rollback:\nbefore %v\nafter  %v", beforeEntries, afterEntries)
	}
	ids := semanticIDs(t, store)
	if ids["a"] != "id_a" || ids["b"] != "id_b" {
		t.Fatalf("released and forgotten rows must be restored, got %v", ids)
	}
}

func TestStickerRoundTripPreservesFields(t *testing.T) {
	store := openTestStore(t)

	e := stickerEntry("alpha", "a", "kitsu_happy")
	e.Tags = []string{"happy", "привет", `quo"te`}
	e.Description = "Happy kitsune <waving>"
	e.Format, e.Available, e.Enabled, e.Position = "video", false, false, 7
	mustReplace(t, store, StickerSetWrite{Set: stickerSet("alpha"), Entries: []StickerEntryRow{e}})

	sets, entries, err := store.LoadStickerCatalog(context.Background())
	if err != nil || len(sets) != 1 || len(entries) != 1 {
		t.Fatalf("load: sets=%v entries=%v err=%v", sets, entries, err)
	}
	if got := fmt.Sprint(entries[0]); got != fmt.Sprint(e) {
		t.Fatalf("round trip mismatch:\nwant %v\ngot  %v", e, entries[0])
	}
	if !sets[0].Enabled || sets[0].Revision != 1 || !sets[0].SyncedAt.Equal(stickerTestTime) {
		t.Fatalf("unexpected set row: %+v", sets[0])
	}
}

func TestStickerLoadFailsOnCorruptRowWithoutLeakingAnnotations(t *testing.T) {
	store := openTestStore(t)
	mustReplace(t, store, StickerSetWrite{Set: stickerSet("alpha"), Entries: []StickerEntryRow{stickerEntry("alpha", "a", "id_a")}})

	if _, err := store.db.ExecContext(context.Background(),
		`UPDATE sticker_entries SET tags = 'not json', description = 'TOPSECRET-DESCRIPTION' WHERE file_unique_id = 'a'`); err != nil {
		t.Fatalf("corrupt row: %v", err)
	}

	_, _, err := store.LoadStickerCatalog(context.Background())
	if err == nil {
		t.Fatalf("expected a corrupt row to fail the load")
	}
	if msg := err.Error(); strings.Contains(msg, "TOPSECRET") || strings.Contains(msg, "not json") || !strings.Contains(msg, "alpha/a") {
		t.Fatalf("error must name the sticker identity only, got %q", msg)
	}
}
