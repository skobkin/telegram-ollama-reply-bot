package migrations

import (
	"context"
	"path/filepath"
	"testing"

	"database/sql"
)

// legacyV11Database builds a file-backed database with exactly the pre-v12
// schema (versions 1..11 applied) and seeds one chat settings row, so the
// v12 column additions must not disturb it.
func legacyV11Database(t *testing.T) *sql.DB {
	t.Helper()

	path := filepath.Join(t.TempDir(), "legacy.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	ctx := context.Background()
	for _, migration := range schemaMigrations[:11] {
		if err := applyMigration(ctx, db, migration, nil); err != nil {
			t.Fatalf("apply migration %s: %v", migration.name, err)
		}
	}

	if _, err := db.ExecContext(ctx, `
INSERT INTO chat_settings(chat_id, character_name, language, gender, interactivity_mode, auto_summary_enabled, preview_mode, keep_tool_history, updated_at, updated_by)
VALUES(123, 'Mira', '', '', '', NULL, '', NULL, CURRENT_TIMESTAMP, 42)
`); err != nil {
		t.Fatalf("seed chat settings: %v", err)
	}

	return db
}

func TestV12MigratesLegacySettingsPreservingBehavior(t *testing.T) {
	ctx := context.Background()
	db := legacyV11Database(t)

	if err := Apply(ctx, db, nil); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}

	var version int
	if err := db.QueryRowContext(ctx, `PRAGMA user_version;`).Scan(&version); err != nil {
		t.Fatalf("read schema version: %v", err)
	}
	if version != targetSchemaVersion {
		t.Fatalf("expected schema version %d, got %d", targetSchemaVersion, version)
	}

	// Global defaults: models unset, proactive participation off. Existing
	// deployments must behave exactly as before the migration.
	var chatModel, summarizeModel, imageModel, systemOneModel, profile, instructions string
	if err := db.QueryRowContext(ctx, `
SELECT chat_model, summarize_model, image_recognition_model, systemone_model,
       default_interaction_profile, decision_instructions
FROM global_settings WHERE id = 1
`).Scan(&chatModel, &summarizeModel, &imageModel, &systemOneModel, &profile, &instructions); err != nil {
		t.Fatalf("read migrated global settings: %v", err)
	}
	if chatModel != "" || summarizeModel != "" || imageModel != "" || systemOneModel != "" || instructions != "" {
		t.Fatalf("expected empty model and instruction defaults, got chat=%q summarize=%q image=%q so=%q instructions=%q",
			chatModel, summarizeModel, imageModel, systemOneModel, instructions)
	}
	if profile != "non_interactive" {
		t.Fatalf("expected non_interactive default profile, got %q", profile)
	}

	// The pre-existing chat row survives with inherit ('') values in the new
	// columns and untouched values in the old ones.
	var characterName, chatProfile string
	if err := db.QueryRowContext(ctx, `
SELECT character_name, interaction_profile FROM chat_settings WHERE chat_id = 123
`).Scan(&characterName, &chatProfile); err != nil {
		t.Fatalf("read migrated chat settings: %v", err)
	}
	if characterName != "Mira" {
		t.Fatalf("expected pre-existing character name to survive, got %q", characterName)
	}
	if chatProfile != "" {
		t.Fatalf("expected empty (inherit) interaction profile, got %q", chatProfile)
	}
}

func TestV12MigrationIsIdempotent(t *testing.T) {
	ctx := context.Background()
	db := legacyV11Database(t)

	if err := Apply(ctx, db, nil); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	if err := Apply(ctx, db, nil); err != nil {
		t.Fatalf("re-apply migrations: %v", err)
	}
}
