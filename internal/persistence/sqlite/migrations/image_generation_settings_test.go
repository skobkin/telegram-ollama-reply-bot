package migrations

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

// legacyV15Database builds a file-backed database with exactly the pre-v16
// schema plus one chat override row.
func legacyV15Database(t *testing.T) *sql.DB {
	t.Helper()

	path := filepath.Join(t.TempDir(), "legacy.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	ctx := context.Background()
	for _, migration := range schemaMigrations[:15] {
		if err := applyMigration(ctx, db, migration, nil); err != nil {
			t.Fatalf("apply migration %s: %v", migration.name, err)
		}
	}

	if _, err := db.ExecContext(ctx, `INSERT INTO chat_settings(chat_id, updated_at, updated_by) VALUES(?, CURRENT_TIMESTAMP, 42)`, int64(-100123)); err != nil {
		t.Fatalf("seed chat settings: %v", err)
	}

	return db
}

// TestV16LeavesImageGenerationOffUntilConfiguredForExistingData verifies the
// rollout is behavior-preserving: the global permission defaults on (the
// environment backend is the real opt-in), the model is empty so the tool
// stays unavailable, and existing chats inherit.
func TestV16LeavesImageGenerationOffUntilConfiguredForExistingData(t *testing.T) {
	ctx := context.Background()
	db := legacyV15Database(t)

	if err := Apply(ctx, db, nil); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}

	var enabled int
	var model string
	if err := db.QueryRowContext(ctx, `SELECT image_generation_enabled, image_generation_model FROM global_settings WHERE id = 1`).Scan(&enabled, &model); err != nil {
		t.Fatalf("read global image generation settings: %v", err)
	}
	if enabled != 1 {
		t.Fatalf("expected global image generation default on (1), got %d", enabled)
	}
	if model != "" {
		t.Fatalf("expected an empty image generation model, got %q", model)
	}

	var chat sql.NullInt64
	if err := db.QueryRowContext(ctx, `SELECT image_generation_enabled FROM chat_settings WHERE chat_id = -100123`).Scan(&chat); err != nil {
		t.Fatalf("read chat image_generation_enabled: %v", err)
	}
	if chat.Valid {
		t.Fatalf("expected chat image_generation_enabled NULL (inherit), got %d", chat.Int64)
	}

	var version int
	if err := db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatalf("read schema version: %v", err)
	}
	if version != targetSchemaVersion {
		t.Fatalf("expected schema version %d, got %d", targetSchemaVersion, version)
	}
}

// TestV16IsIdempotent re-applies the step over an already migrated schema.
func TestV16IsIdempotent(t *testing.T) {
	ctx := context.Background()
	db := legacyV15Database(t)

	if err := Apply(ctx, db, nil); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := migrateV16AddImageGenerationSettings(ctx, tx, nil); err != nil {
		t.Fatalf("re-applying v16 must be a no-op, got: %v", err)
	}
}
