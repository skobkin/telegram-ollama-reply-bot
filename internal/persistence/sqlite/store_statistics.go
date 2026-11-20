package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// StatisticsQuarantineSuffix names the single overwritten quarantine slot of
// a dataset: "<dataset>.unreadable".
const StatisticsQuarantineSuffix = ".unreadable"

// StatisticsSnapshotMeta is the blob-free header of one persisted dataset.
// It is read first so the caller can classify the format version, and the
// payload size, before any blob is loaded.
type StatisticsSnapshotMeta struct {
	FormatVersion int
	Generation    uint64
	Revision      uint64
	PayloadBytes  int64
	SavedAt       time.Time
}

// StatisticsSnapshotRow is one dataset's whole persisted snapshot.
type StatisticsSnapshotRow struct {
	Dataset       string
	FormatVersion int
	Generation    uint64
	Revision      uint64
	Payload       []byte
	SavedAt       time.Time
}

// LoadStatisticsSnapshotMeta reads the header of a dataset row WITHOUT its
// payload. ok is false when the dataset has no row.
func (s *Store) LoadStatisticsSnapshotMeta(ctx context.Context, dataset string) (meta StatisticsSnapshotMeta, ok bool, err error) {
	var (
		generation, revision int64
		savedAt              string
	)
	err = s.db.QueryRowContext(ctx,
		`SELECT format_version, generation, revision, length(payload), saved_at FROM statistics_snapshots WHERE dataset = ?`,
		dataset,
	).Scan(&meta.FormatVersion, &generation, &revision, &meta.PayloadBytes, &savedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return StatisticsSnapshotMeta{}, false, nil
	}
	if err != nil {
		return StatisticsSnapshotMeta{}, false, fmt.Errorf("load statistics snapshot meta: %w", err)
	}
	meta.Generation, meta.Revision = uint64(generation), uint64(revision) //nolint:gosec // generation/revision counters never approach the int64 range
	meta.SavedAt = parseSQLiteTime(savedAt)

	return meta, true, nil
}

// LoadStatisticsSnapshotPayload reads the payload blob of a dataset row.
func (s *Store) LoadStatisticsSnapshotPayload(ctx context.Context, dataset string) ([]byte, error) {
	var payload []byte
	if err := s.db.QueryRowContext(ctx,
		`SELECT payload FROM statistics_snapshots WHERE dataset = ?`, dataset,
	).Scan(&payload); err != nil {
		return nil, fmt.Errorf("load statistics snapshot payload: %w", err)
	}

	return payload, nil
}

// SaveStatisticsSnapshot is a conditional upsert: a row is written only when
// its (generation, revision) pair is strictly greater than the stored one.
// The database itself therefore rejects delayed or stale writes (for example
// a flush captured before an administrative reset). applied is false when a
// row exists with an equal or newer pair.
func (s *Store) SaveStatisticsSnapshot(ctx context.Context, row StatisticsSnapshotRow) (applied bool, err error) {
	result, err := s.db.ExecContext(ctx, `
INSERT INTO statistics_snapshots (dataset, format_version, generation, revision, payload, saved_at)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT(dataset) DO UPDATE SET
  format_version = excluded.format_version,
  generation = excluded.generation,
  revision = excluded.revision,
  payload = excluded.payload,
  saved_at = excluded.saved_at
WHERE (excluded.generation, excluded.revision) > (statistics_snapshots.generation, statistics_snapshots.revision)`,
		row.Dataset, row.FormatVersion, int64(row.Generation), int64(row.Revision), row.Payload, formatSQLiteTime(row.SavedAt), //nolint:gosec // generation/revision counters never approach the int64 range
	)
	if err != nil {
		return false, fmt.Errorf("save statistics snapshot: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("save statistics snapshot: rows affected: %w", err)
	}

	return affected > 0, nil
}

// QuarantineStatisticsSnapshot moves an unreadable dataset row to its
// "<dataset>.unreadable" slot atomically: the copy happens server-side (no
// blob round-trip) and the original is deleted in the same transaction. On
// any failure nothing changes and an error is returned, so the caller never
// replaces a row that was not safely set aside.
func (s *Store) QuarantineStatisticsSnapshot(ctx context.Context, dataset string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin statistics quarantine: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `
INSERT OR REPLACE INTO statistics_snapshots (dataset, format_version, generation, revision, payload, saved_at)
SELECT ? , format_version, generation, revision, payload, saved_at FROM statistics_snapshots WHERE dataset = ?`,
		dataset+StatisticsQuarantineSuffix, dataset,
	); err != nil {
		return fmt.Errorf("copy statistics snapshot to quarantine: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM statistics_snapshots WHERE dataset = ?`, dataset); err != nil {
		return fmt.Errorf("delete quarantined statistics snapshot: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit statistics quarantine: %w", err)
	}

	return nil
}
