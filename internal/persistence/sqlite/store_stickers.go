package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// ErrStickerSetConflict reports that a sticker set changed (or appeared)
// since the caller read its revision. Nothing was written.
var ErrStickerSetConflict = errors.New("sticker set changed concurrently")

// StickerSetRow is one persisted sticker set. Revision is the optimistic
// write guard: a set is created at revision 1 and every committed write
// increments it.
type StickerSetRow struct {
	Name      string
	Title     string
	Enabled   bool
	Revision  int64
	SyncedAt  time.Time
	CreatedAt time.Time
	UpdatedAt time.Time
}

// StickerEntryRow is one persisted sticker, identified by
// (SetName, FileUniqueID). SemanticID == "" means unassigned and is stored as
// SQL NULL, never as an empty string.
type StickerEntryRow struct {
	SetName      string
	FileUniqueID string
	FileID       string
	SemanticID   string
	Description  string
	Tags         []string
	Emoji        string
	Format       string
	Position     int
	Available    bool
	Enabled      bool
	SyncedAt     time.Time
	UpdatedAt    time.Time
}

// StickerSetWrite is one atomic catalog update of a single set.
//
// ExpectedRevision 0 means the set must not exist yet (first import);
// otherwise it must equal the stored revision. Set.Revision is ignored.
//
// Forgotten lists file_unique_ids whose rows are deleted. Release lists
// surviving rows whose semantic_id is cleared before the final upserts: SQLite
// checks the unique index per statement, so a valid final catalog (a swap, a
// reset ID reused, a forgotten ID reused) could otherwise collide while the
// upserts are applied one by one.
type StickerSetWrite struct {
	Set              StickerSetRow
	ExpectedRevision int64
	Entries          []StickerEntryRow
	Forgotten        []string
	Release          []string
}

// LoadStickerCatalog reads every persisted sticker set and entry — disabled,
// incomplete and unavailable rows included — so the caller holds the complete
// catalog. A row that cannot be decoded fails the load; the error names the
// set and sticker identity only, never annotation text.
func (s *Store) LoadStickerCatalog(ctx context.Context) ([]StickerSetRow, []StickerEntryRow, error) {
	setRows, err := s.db.QueryContext(ctx,
		`SELECT name, title, enabled, revision, synced_at, created_at, updated_at FROM sticker_sets ORDER BY name`)
	if err != nil {
		return nil, nil, fmt.Errorf("query sticker sets: %w", err)
	}
	defer func() { _ = setRows.Close() }()

	var sets []StickerSetRow
	for setRows.Next() {
		var (
			row                          StickerSetRow
			enabled                      int
			syncedAt, createdAt, updated string
		)
		if err := setRows.Scan(&row.Name, &row.Title, &enabled, &row.Revision, &syncedAt, &createdAt, &updated); err != nil {
			return nil, nil, fmt.Errorf("scan sticker set: %w", err)
		}
		row.Enabled = enabled != 0
		row.SyncedAt, row.CreatedAt, row.UpdatedAt = parseSQLiteTime(syncedAt), parseSQLiteTime(createdAt), parseSQLiteTime(updated)
		sets = append(sets, row)
	}
	if err := setRows.Err(); err != nil {
		return nil, nil, fmt.Errorf("iterate sticker sets: %w", err)
	}

	entryRows, err := s.db.QueryContext(ctx, `
SELECT set_name, file_unique_id, file_id, semantic_id, description, tags, emoji, format, position, available, enabled, synced_at, updated_at
FROM sticker_entries ORDER BY set_name, position, file_unique_id`)
	if err != nil {
		return nil, nil, fmt.Errorf("query sticker entries: %w", err)
	}
	defer func() { _ = entryRows.Close() }()

	var entries []StickerEntryRow
	for entryRows.Next() {
		var (
			row               StickerEntryRow
			semantic          sql.NullString
			tags              string
			available, enable int
			syncedAt, updated string
		)
		if err := entryRows.Scan(&row.SetName, &row.FileUniqueID, &row.FileID, &semantic, &row.Description, &tags,
			&row.Emoji, &row.Format, &row.Position, &available, &enable, &syncedAt, &updated); err != nil {
			return nil, nil, fmt.Errorf("scan sticker entry: %w", err)
		}
		if err := json.Unmarshal([]byte(tags), &row.Tags); err != nil {
			return nil, nil, fmt.Errorf("sticker %s/%s: tags are not a JSON string array", row.SetName, row.FileUniqueID)
		}
		row.SemanticID = semantic.String
		row.Available, row.Enabled = available != 0, enable != 0
		row.SyncedAt, row.UpdatedAt = parseSQLiteTime(syncedAt), parseSQLiteTime(updated)
		entries = append(entries, row)
	}
	if err := entryRows.Err(); err != nil {
		return nil, nil, fmt.Errorf("iterate sticker entries: %w", err)
	}

	return sets, entries, nil
}

// ReplaceStickerSet applies one set update in a single transaction:
//
//  1. guard + write the set row (revision check),
//  2. delete forgotten rows,
//  3. release changed semantic IDs (set to NULL),
//  4. upsert every entry with its final values.
//
// Any failure — including an unexpected unique violation — rolls everything
// back. Rows are never deleted wholesale.
func (s *Store) ReplaceStickerSet(ctx context.Context, w StickerSetWrite) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin sticker set write: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	set := w.Set
	if w.ExpectedRevision == 0 {
		res, err := tx.ExecContext(ctx, `
INSERT INTO sticker_sets (name, title, enabled, revision, synced_at, created_at, updated_at)
VALUES (?, ?, ?, 1, ?, ?, ?) ON CONFLICT(name) DO NOTHING`,
			set.Name, set.Title, boolToSQLite(set.Enabled),
			formatSQLiteTime(set.SyncedAt), formatSQLiteTime(set.CreatedAt), formatSQLiteTime(set.UpdatedAt))
		if err := requireOneRow(res, err, "insert sticker set"); err != nil {
			return err
		}
	} else {
		res, err := tx.ExecContext(ctx, `
UPDATE sticker_sets SET title = ?, enabled = ?, synced_at = ?, updated_at = ?, revision = revision + 1
WHERE name = ? AND revision = ?`,
			set.Title, boolToSQLite(set.Enabled), formatSQLiteTime(set.SyncedAt), formatSQLiteTime(set.UpdatedAt),
			set.Name, w.ExpectedRevision)
		if err := requireOneRow(res, err, "update sticker set"); err != nil {
			return err
		}
	}

	for _, unique := range w.Forgotten {
		if _, err := tx.ExecContext(ctx, `DELETE FROM sticker_entries WHERE set_name = ? AND file_unique_id = ?`, set.Name, unique); err != nil {
			return fmt.Errorf("delete forgotten sticker: %w", err)
		}
	}
	for _, unique := range w.Release {
		if _, err := tx.ExecContext(ctx, `UPDATE sticker_entries SET semantic_id = NULL WHERE set_name = ? AND file_unique_id = ?`, set.Name, unique); err != nil {
			return fmt.Errorf("release sticker semantic id: %w", err)
		}
	}
	for i := range w.Entries {
		if err := upsertStickerEntry(ctx, tx, set.Name, &w.Entries[i]); err != nil {
			return err
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit sticker set write: %w", err)
	}

	return nil
}

func upsertStickerEntry(ctx context.Context, tx *sql.Tx, setName string, e *StickerEntryRow) error {
	tags := e.Tags
	if tags == nil {
		tags = []string{}
	}
	encodedTags, err := json.Marshal(tags)
	if err != nil {
		return fmt.Errorf("encode sticker tags: %w", err)
	}
	// An unassigned semantic ID is SQL NULL, never an empty string.
	semantic := sql.NullString{String: e.SemanticID, Valid: e.SemanticID != ""}

	if _, err := tx.ExecContext(ctx, `
INSERT INTO sticker_entries
  (set_name, file_unique_id, file_id, semantic_id, description, tags, emoji, format, position, available, enabled, synced_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(set_name, file_unique_id) DO UPDATE SET
  file_id = excluded.file_id, semantic_id = excluded.semantic_id, description = excluded.description,
  tags = excluded.tags, emoji = excluded.emoji, format = excluded.format, position = excluded.position,
  available = excluded.available, enabled = excluded.enabled, synced_at = excluded.synced_at, updated_at = excluded.updated_at`,
		setName, e.FileUniqueID, e.FileID, semantic, e.Description, string(encodedTags), e.Emoji, e.Format, e.Position,
		boolToSQLite(e.Available), boolToSQLite(e.Enabled), formatSQLiteTime(e.SyncedAt), formatSQLiteTime(e.UpdatedAt),
	); err != nil {
		return fmt.Errorf("upsert sticker %s/%s: %w", setName, e.FileUniqueID, err)
	}

	return nil
}

// SetStickerSetEnabled flips a set's enabled flag, guarded by the revision.
func (s *Store) SetStickerSetEnabled(ctx context.Context, name string, enabled bool, expectedRevision int64, at time.Time) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE sticker_sets SET enabled = ?, updated_at = ?, revision = revision + 1 WHERE name = ? AND revision = ?`,
		boolToSQLite(enabled), formatSQLiteTime(at), name, expectedRevision)

	return requireOneRow(res, err, "update sticker set enabled flag")
}

// DeleteStickerSet removes a set and (by cascade) all its entries, guarded by
// the revision.
func (s *Store) DeleteStickerSet(ctx context.Context, name string, expectedRevision int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM sticker_sets WHERE name = ? AND revision = ?`, name, expectedRevision)

	return requireOneRow(res, err, "delete sticker set")
}

// requireOneRow maps "no row matched the revision guard" to
// ErrStickerSetConflict and passes any other failure through wrapped.
func requireOneRow(res sql.Result, err error, what string) error {
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("%s: rows affected: %w", what, err)
	}
	if affected != 1 {
		return ErrStickerSetConflict
	}

	return nil
}
