package sqlite

import (
	"context"
	"testing"
	"time"
)

func statsRow(dataset string, generation, revision uint64, payload string) StatisticsSnapshotRow {
	return StatisticsSnapshotRow{
		Dataset:       dataset,
		FormatVersion: 1,
		Generation:    generation,
		Revision:      revision,
		Payload:       []byte(payload),
		SavedAt:       time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC),
	}
}

func TestStatisticsSnapshotLoadWhenMissing(t *testing.T) {
	store := openTestStore(t)

	_, ok, err := store.LoadStatisticsSnapshotMeta(context.Background(), "interaction")
	if err != nil || ok {
		t.Fatalf("missing dataset: ok=%v err=%v", ok, err)
	}
}

func TestStatisticsSnapshotSaveReplaceAndMetaWithoutBlob(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	applied, err := store.SaveStatisticsSnapshot(ctx, statsRow("interaction", 0, 1, "first"))
	if err != nil || !applied {
		t.Fatalf("first save: applied=%v err=%v", applied, err)
	}
	applied, err = store.SaveStatisticsSnapshot(ctx, statsRow("interaction", 0, 2, "second-payload"))
	if err != nil || !applied {
		t.Fatalf("replace: applied=%v err=%v", applied, err)
	}

	meta, ok, err := store.LoadStatisticsSnapshotMeta(ctx, "interaction")
	if err != nil || !ok {
		t.Fatalf("meta: ok=%v err=%v", ok, err)
	}
	if meta.Generation != 0 || meta.Revision != 2 || meta.FormatVersion != 1 || meta.PayloadBytes != int64(len("second-payload")) {
		t.Fatalf("unexpected meta %+v", meta)
	}
	if !meta.SavedAt.Equal(time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("unexpected saved_at %v", meta.SavedAt)
	}

	payload, err := store.LoadStatisticsSnapshotPayload(ctx, "interaction")
	if err != nil || string(payload) != "second-payload" {
		t.Fatalf("payload %q err=%v", payload, err)
	}
}

func TestStatisticsSnapshotConditionalUpsertRejectsStalePairs(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	if _, err := store.SaveStatisticsSnapshot(ctx, statsRow("tool_usage", 2, 10, "gen2")); err != nil {
		t.Fatal(err)
	}

	for name, row := range map[string]StatisticsSnapshotRow{
		"older generation, higher revision": statsRow("tool_usage", 1, 99, "stale-gen"),
		"same pair":                         statsRow("tool_usage", 2, 10, "dup"),
		"same generation, lower revision":   statsRow("tool_usage", 2, 9, "stale-rev"),
	} {
		applied, err := store.SaveStatisticsSnapshot(ctx, row)
		if err != nil || applied {
			t.Fatalf("%s: applied=%v err=%v", name, applied, err)
		}
	}
	payload, _ := store.LoadStatisticsSnapshotPayload(ctx, "tool_usage")
	if string(payload) != "gen2" {
		t.Fatalf("stale writes must not change the row, got %q", payload)
	}

	// A newer generation wins even with a LOWER revision (a reset).
	applied, err := store.SaveStatisticsSnapshot(ctx, statsRow("tool_usage", 3, 0, "reset"))
	if err != nil || !applied {
		t.Fatalf("reset generation must win: applied=%v err=%v", applied, err)
	}
}

func TestStatisticsSnapshotQuarantineIsAtomic(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	if _, err := store.SaveStatisticsSnapshot(ctx, statsRow("interaction", 4, 4, "unreadable")); err != nil {
		t.Fatal(err)
	}
	if err := store.QuarantineStatisticsSnapshot(ctx, "interaction"); err != nil {
		t.Fatalf("quarantine: %v", err)
	}
	if _, ok, _ := store.LoadStatisticsSnapshotMeta(ctx, "interaction"); ok {
		t.Fatal("the original row must be gone")
	}
	payload, err := store.LoadStatisticsSnapshotPayload(ctx, "interaction"+StatisticsQuarantineSuffix)
	if err != nil || string(payload) != "unreadable" {
		t.Fatalf("quarantine slot payload %q err=%v", payload, err)
	}

	// A fresh dataset can be saved again even though its pair restarts low,
	// and a second quarantine overwrites the single slot.
	if applied, err := store.SaveStatisticsSnapshot(ctx, statsRow("interaction", 0, 1, "fresh")); err != nil || !applied {
		t.Fatalf("fresh save: applied=%v err=%v", applied, err)
	}
	if err := store.QuarantineStatisticsSnapshot(ctx, "interaction"); err != nil {
		t.Fatal(err)
	}
	payload, _ = store.LoadStatisticsSnapshotPayload(ctx, "interaction"+StatisticsQuarantineSuffix)
	if string(payload) != "fresh" {
		t.Fatalf("the slot must be overwritten, got %q", payload)
	}
}

func TestStatisticsSnapshotQuarantineFailureLeavesOriginalIntact(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	if _, err := store.SaveStatisticsSnapshot(ctx, statsRow("interaction", 1, 1, "keep-me")); err != nil {
		t.Fatal(err)
	}
	// Force the DELETE step to fail inside the transaction: a trigger aborts it.
	if _, err := store.db.ExecContext(ctx, `
CREATE TRIGGER block_delete BEFORE DELETE ON statistics_snapshots
BEGIN SELECT RAISE(ABORT, 'delete blocked'); END`); err != nil {
		t.Fatal(err)
	}

	if err := store.QuarantineStatisticsSnapshot(ctx, "interaction"); err == nil {
		t.Fatal("expected a quarantine failure")
	}
	payload, err := store.LoadStatisticsSnapshotPayload(ctx, "interaction")
	if err != nil || string(payload) != "keep-me" {
		t.Fatalf("original row must be intact, got %q err=%v", payload, err)
	}
	if _, ok, _ := store.LoadStatisticsSnapshotMeta(ctx, "interaction"+StatisticsQuarantineSuffix); ok {
		t.Fatal("the rolled-back copy must not leave a quarantine row")
	}
}
