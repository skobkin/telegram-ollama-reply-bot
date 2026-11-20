package migrations

import (
	"context"
	"database/sql"
	"log/slog"
)

// migrateV14AddStickerCatalog creates the operator-curated sticker catalog.
//
// sticker_sets holds one row per imported Telegram sticker set; revision is
// the optimistic write guard (a set row is created at revision 1 and every
// committed write increments it). sticker_entries identifies a sticker by
// (set_name, file_unique_id), the media identity that survives re-fetches;
// file_id is only the current send token and position is a display aid, never
// identity.
//
// semantic_id is NULL for an unannotated sticker: the partial unique index
// covers only assigned IDs, so any number of unannotated rows coexist. The
// CHECK keeps an empty string from ever standing in for "unassigned".
func migrateV14AddStickerCatalog(ctx context.Context, tx *sql.Tx, log *slog.Logger) error {
	return applyStatements(ctx, tx, "add_sticker_catalog", []string{
		`CREATE TABLE IF NOT EXISTS sticker_sets (
  name TEXT PRIMARY KEY,
  title TEXT NOT NULL,
  enabled INTEGER NOT NULL DEFAULT 1,
  revision INTEGER NOT NULL DEFAULT 1,
  synced_at TEXT NOT NULL,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);`,
		`CREATE TABLE IF NOT EXISTS sticker_entries (
  set_name TEXT NOT NULL REFERENCES sticker_sets(name) ON DELETE CASCADE,
  file_unique_id TEXT NOT NULL,
  file_id TEXT NOT NULL,
  semantic_id TEXT NULL CHECK (semantic_id IS NULL OR length(semantic_id) > 0),
  description TEXT NOT NULL DEFAULT '',
  tags TEXT NOT NULL DEFAULT '[]',
  emoji TEXT NOT NULL DEFAULT '',
  format TEXT NOT NULL CHECK (format IN ('static', 'animated', 'video')),
  position INTEGER NOT NULL,
  available INTEGER NOT NULL,
  enabled INTEGER NOT NULL DEFAULT 1,
  synced_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  PRIMARY KEY (set_name, file_unique_id)
);`,
		`CREATE UNIQUE INDEX IF NOT EXISTS sticker_entries_semantic_id
  ON sticker_entries(semantic_id) WHERE semantic_id IS NOT NULL;`,
	}, log)
}
