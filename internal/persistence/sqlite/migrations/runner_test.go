package migrations

import (
	"bytes"
	"context"
	"database/sql"
	"log/slog"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

func TestApplyBootstrapsAdminConfigSchema(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", "file::memory:?cache=shared")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if err := Apply(ctx, db, nil); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}

	for _, table := range []string{"global_settings", "chat_settings", "prompt_templates", "chat_catalog", "chat_whitelist"} {
		var count int
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&count); err != nil {
			t.Fatalf("query table %s: %v", table, err)
		}
		if count != 1 {
			t.Fatalf("expected table %s", table)
		}
	}

	for _, column := range []string{"language", "gender"} {
		exists, err := columnExists(ctx, mustBeginTx(t, db), "chat_settings", column)
		if err != nil {
			t.Fatalf("check column %s: %v", column, err)
		}
		if !exists {
			t.Fatalf("expected chat_settings.%s column", column)
		}
	}
}

func TestApplySeedsGlobalDefaults(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", "file::memory:?cache=shared")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if err := Apply(ctx, db, nil); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}

	var count int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM prompt_templates WHERE scope_type='global' AND scope_id=0`).Scan(&count); err != nil {
		t.Fatalf("count prompt templates: %v", err)
	}
	if count != 4 {
		t.Fatalf("expected 4 prompt templates, got %d", count)
	}
}

func TestApplyLogsEachMigrationAtInfo(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", "file::memory:?cache=shared")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	if err := Apply(ctx, db, logger); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, "msg=\"applying sqlite migration\"") || !strings.Contains(output, "name=bootstrap_admin_config") || !strings.Contains(output, "name=add_chat_language_and_gender") {
		t.Fatalf("expected migration log output, got %q", output)
	}
}

func TestApplyLogsMigrationDetailsAtDebug(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", "file::memory:?cache=shared")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	if err := Apply(ctx, db, logger); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, "msg=\"loaded sqlite schema version\"") || !strings.Contains(output, "msg=\"executing sqlite migration statement\"") {
		t.Fatalf("expected debug migration log output, got %q", output)
	}
}

func mustBeginTx(t *testing.T, db *sql.DB) *sql.Tx {
	t.Helper()

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback() })

	return tx
}
