package migrations

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

// legacyV6Database builds a file-backed database with exactly the pre-v7
// schema (versions 1..6 applied) plus one chat override row.
func legacyV6Database(t *testing.T) *sql.DB {
	t.Helper()

	path := filepath.Join(t.TempDir(), "legacy.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	ctx := context.Background()
	for _, migration := range schemaMigrations[:6] {
		if err := applyMigration(ctx, db, migration, nil); err != nil {
			t.Fatalf("apply migration %s: %v", migration.name, err)
		}
	}

	if _, err := db.ExecContext(ctx, `
INSERT INTO chat_settings(chat_id, character_name, language, gender, tone_mode, allow_teasing, interactivity_mode, updated_at, updated_by)
VALUES(?, '', '', '', '', NULL, '', CURRENT_TIMESTAMP, 42)
`, int64(-100123)); err != nil {
		t.Fatalf("seed chat settings: %v", err)
	}

	return db
}

// TestV7MigratesLegacySchema adds both auto-summary columns; the chat column
// stays NULL (inherit) and the global default lands at 0 (feature off).
func TestV7MigratesLegacySchema(t *testing.T) {
	ctx := context.Background()
	db := legacyV6Database(t)

	if err := Apply(ctx, db, nil); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}

	var autoSummary sql.NullInt64
	if err := db.QueryRowContext(ctx, `SELECT auto_summary_enabled FROM chat_settings WHERE chat_id = -100123`).Scan(&autoSummary); err != nil {
		t.Fatalf("read chat auto_summary_enabled: %v", err)
	}
	if autoSummary.Valid {
		t.Fatalf("expected chat auto_summary_enabled to be NULL after migration, got %d", autoSummary.Int64)
	}

	var globalDefault int
	if err := db.QueryRowContext(ctx, `SELECT default_auto_summary_enabled FROM global_settings WHERE id = 1`).Scan(&globalDefault); err != nil {
		t.Fatalf("read global default_auto_summary_enabled: %v", err)
	}
	if globalDefault != 0 {
		t.Fatalf("expected global default 0 after migration, got %d", globalDefault)
	}
}

// TestV7FreshInstallRunsFullChain runs the whole chain from scratch and
// lands on the target schema version with the feature off everywhere.
func TestV7FreshInstallRunsFullChain(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "fresh.sqlite"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if err := Apply(ctx, db, nil); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}

	var version int
	if err := db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	if version != targetSchemaVersion {
		t.Fatalf("expected schema version %d, got %d", targetSchemaVersion, version)
	}

	var globalDefault int
	if err := db.QueryRowContext(ctx, `SELECT default_auto_summary_enabled FROM global_settings WHERE id = 1`).Scan(&globalDefault); err != nil {
		t.Fatalf("read global default: %v", err)
	}
	if globalDefault != 0 {
		t.Fatalf("expected fresh install to keep the feature off, got %d", globalDefault)
	}
}

// TestV7IsIdempotent proves re-running Apply is a no-op.
func TestV7IsIdempotent(t *testing.T) {
	ctx := context.Background()
	db := legacyV6Database(t)

	if err := Apply(ctx, db, nil); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	if err := Apply(ctx, db, nil); err != nil {
		t.Fatalf("second apply: %v", err)
	}

	var version int
	if err := db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	if version != targetSchemaVersion {
		t.Fatalf("expected schema version %d after re-apply, got %d", targetSchemaVersion, version)
	}
}
