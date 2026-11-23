package migrations

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

// legacyV14Database builds a file-backed database with exactly the pre-v15
// schema plus one chat override row.
func legacyV14Database(t *testing.T) *sql.DB {
	t.Helper()

	path := filepath.Join(t.TempDir(), "legacy.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	ctx := context.Background()
	for _, migration := range schemaMigrations[:14] {
		if err := applyMigration(ctx, db, migration, nil); err != nil {
			t.Fatalf("apply migration %s: %v", migration.name, err)
		}
	}

	if _, err := db.ExecContext(ctx, `INSERT INTO chat_settings(chat_id, updated_at, updated_by) VALUES(?, CURRENT_TIMESTAMP, 42)`, int64(-100123)); err != nil {
		t.Fatalf("seed chat settings: %v", err)
	}

	return db
}

// TestV15KeepsWebSearchOnAndInheritingForExistingData verifies the rollout is
// behavior-preserving: the global default is on and existing chats inherit it.
func TestV15KeepsWebSearchOnAndInheritingForExistingData(t *testing.T) {
	ctx := context.Background()
	db := legacyV14Database(t)

	if err := Apply(ctx, db, nil); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}

	var global int
	if err := db.QueryRowContext(ctx, `SELECT web_search_enabled FROM global_settings WHERE id = 1`).Scan(&global); err != nil {
		t.Fatalf("read global web_search_enabled: %v", err)
	}
	if global != 1 {
		t.Fatalf("expected global web search default on (1), got %d", global)
	}

	var chat sql.NullInt64
	if err := db.QueryRowContext(ctx, `SELECT web_search_enabled FROM chat_settings WHERE chat_id = -100123`).Scan(&chat); err != nil {
		t.Fatalf("read chat web_search_enabled: %v", err)
	}
	if chat.Valid {
		t.Fatalf("expected chat web_search_enabled NULL (inherit), got %d", chat.Int64)
	}

	var version int
	if err := db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatalf("read schema version: %v", err)
	}
	if version != targetSchemaVersion {
		t.Fatalf("expected schema version %d, got %d", targetSchemaVersion, version)
	}
}

// TestV15IsIdempotent re-applies the step over an already migrated schema.
func TestV15IsIdempotent(t *testing.T) {
	ctx := context.Background()
	db := legacyV14Database(t)

	if err := Apply(ctx, db, nil); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := migrateV15AddWebSearchSetting(ctx, tx, nil); err != nil {
		t.Fatalf("re-applying v15 must be a no-op, got: %v", err)
	}
}
