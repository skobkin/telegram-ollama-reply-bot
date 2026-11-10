package migrations

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

// legacyV5Database builds a file-backed database with exactly the pre-v6
// schema (versions 1..5 applied) and returns it together with its path.
func legacyV5Database(t *testing.T, whitelist []int64) *sql.DB {
	t.Helper()

	path := filepath.Join(t.TempDir(), "legacy.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	ctx := context.Background()
	for _, migration := range schemaMigrations[:5] {
		if err := applyMigration(ctx, db, migration, nil); err != nil {
			t.Fatalf("apply migration %s: %v", migration.name, err)
		}
	}

	for _, chatID := range whitelist {
		if _, err := db.ExecContext(ctx, `
INSERT INTO chat_whitelist(chat_id, added_at, added_by)
VALUES(?, CURRENT_TIMESTAMP, 42)
`, chatID); err != nil {
			t.Fatalf("seed whitelist chat %d: %v", chatID, err)
		}
	}

	return db
}

// TestV6MigratesLegacyEmptyWhitelistToAllowlist is the documented breaking
// change: an install whose empty whitelist previously meant "allow everyone"
// lands on allowlist (deny all ordinary chats) — never allow_all.
func TestV6MigratesLegacyEmptyWhitelistToAllowlist(t *testing.T) {
	ctx := context.Background()
	db := legacyV5Database(t, nil)

	if err := Apply(ctx, db, nil); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}

	var policy string
	if err := db.QueryRowContext(ctx, `SELECT access_policy FROM global_settings WHERE id = 1`).Scan(&policy); err != nil {
		t.Fatalf("read access policy: %v", err)
	}
	if policy != "allowlist" {
		t.Fatalf("expected allowlist policy, got %q", policy)
	}

	var count int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM chat_whitelist`).Scan(&count); err != nil {
		t.Fatalf("count whitelist: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected empty whitelist to stay empty, got %d rows", count)
	}
}

// TestV6PreservesNonEmptyWhitelist proves populated installs keep their
// entries and their effective behavior (allowlist + membership = allowed).
func TestV6PreservesNonEmptyWhitelist(t *testing.T) {
	ctx := context.Background()
	db := legacyV5Database(t, []int64{-1001234567890, 55})

	if err := Apply(ctx, db, nil); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}

	var policy string
	if err := db.QueryRowContext(ctx, `SELECT access_policy FROM global_settings WHERE id = 1`).Scan(&policy); err != nil {
		t.Fatalf("read access policy: %v", err)
	}
	if policy != "allowlist" {
		t.Fatalf("expected allowlist policy, got %q", policy)
	}

	rows, err := db.QueryContext(ctx, `SELECT chat_id FROM chat_whitelist ORDER BY chat_id ASC`)
	if err != nil {
		t.Fatalf("list whitelist: %v", err)
	}
	defer func() { _ = rows.Close() }()

	var got []int64
	for rows.Next() {
		var chatID int64
		if err := rows.Scan(&chatID); err != nil {
			t.Fatalf("scan whitelist chat: %v", err)
		}
		got = append(got, chatID)
	}
	if len(got) != 2 || got[0] != -1001234567890 || got[1] != 55 {
		t.Fatalf("expected whitelist entries preserved, got %v", got)
	}
}

// TestV6FreshInstallLandsOnAllowlist runs the full chain from scratch.
func TestV6FreshInstallLandsOnAllowlist(t *testing.T) {
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

	var policy string
	if err := db.QueryRowContext(ctx, `SELECT access_policy FROM global_settings WHERE id = 1`).Scan(&policy); err != nil {
		t.Fatalf("read access policy: %v", err)
	}
	if policy != "allowlist" {
		t.Fatalf("expected allowlist policy on fresh install, got %q", policy)
	}
}

// TestV6IsIdempotent proves a second run is a no-op.
func TestV6IsIdempotent(t *testing.T) {
	ctx := context.Background()
	db := legacyV5Database(t, []int64{7})

	if err := Apply(ctx, db, nil); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	if err := Apply(ctx, db, nil); err != nil {
		t.Fatalf("second apply: %v", err)
	}

	var policy string
	if err := db.QueryRowContext(ctx, `SELECT access_policy FROM global_settings WHERE id = 1`).Scan(&policy); err != nil {
		t.Fatalf("read access policy: %v", err)
	}
	if policy != "allowlist" {
		t.Fatalf("expected allowlist policy after re-apply, got %q", policy)
	}
}
