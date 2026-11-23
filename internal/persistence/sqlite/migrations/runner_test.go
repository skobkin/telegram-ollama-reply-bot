package migrations

import (
	"bytes"
	"context"
	"database/sql"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"telegram-ollama-reply-bot/internal/llm"

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

	for _, table := range []string{"global_settings", "chat_settings", "prompt_templates", "chat_catalog", "chat_whitelist", "reminders"} {
		var count int
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&count); err != nil {
			t.Fatalf("query table %s: %v", table, err)
		}
		if count != 1 {
			t.Fatalf("expected table %s", table)
		}
	}

	for _, column := range []string{"character_name", "language", "gender"} {
		exists, err := chatSettingsColumnExists(ctx, mustBeginTx(t, db), column)
		if err != nil {
			t.Fatalf("check column %s: %v", column, err)
		}
		if !exists {
			t.Fatalf("expected chat_settings.%s column", column)
		}
	}
	aliasExists, err := chatSettingsColumnExists(ctx, mustBeginTx(t, db), "alias")
	if err != nil {
		t.Fatalf("check alias column: %v", err)
	}
	if aliasExists {
		t.Fatalf("did not expect chat_settings.alias column")
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
	if count != 3 {
		t.Fatalf("expected 3 prompt templates, got %d", count)
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
	if !strings.Contains(output, "msg=\"applying sqlite migration\"") || !strings.Contains(output, "name=bootstrap_admin_config") || !strings.Contains(output, "name=add_chat_language_and_gender") || !strings.Contains(output, "name=add_reminders") || !strings.Contains(output, "name=make_tools_mandatory_for_chat") || !strings.Contains(output, "name=rename_chat_alias_to_character_name") || !strings.Contains(output, "name=refresh_chat_prompt_image_section") || !strings.Contains(output, "name=remove_tone_and_teasing_prompt_params") {
		t.Fatalf("expected migration log output, got %q", output)
	}
}

func TestApplyUpgradesOnlyOldDefaultChatPrompt(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", "file::memory:?cache=shared")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	for _, migration := range schemaMigrations[:3] {
		if err := applyMigration(ctx, db, migration, nil); err != nil {
			t.Fatalf("apply migration %s: %v", migration.name, err)
		}
	}
	if _, err := db.ExecContext(ctx, `
UPDATE prompt_templates
SET template_text = ?
WHERE scope_type = 'global' AND scope_id = 0 AND feature = 'chat'
`, llm.OldDefaultChatPromptTemplate); err != nil {
		t.Fatalf("set old default chat prompt: %v", err)
	}

	if err := Apply(ctx, db, nil); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}

	var body string
	if err := db.QueryRowContext(ctx, `
SELECT template_text FROM prompt_templates
WHERE scope_type = 'global' AND scope_id = 0 AND feature = 'chat'
`).Scan(&body); err != nil {
		t.Fatalf("read chat prompt: %v", err)
	}
	if body != llm.DefaultChatPromptTemplate {
		t.Fatalf("expected upgraded default chat prompt")
	}
}

func TestApplyRefreshesStaleImageSectionChatPrompt(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", "file::memory:?cache=shared")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	for _, migration := range schemaMigrations[:9] {
		if err := applyMigration(ctx, db, migration, nil); err != nil {
			t.Fatalf("apply migration %s: %v", migration.name, err)
		}
	}
	if _, err := db.ExecContext(ctx, `
UPDATE prompt_templates
SET template_text = ?
WHERE scope_type = 'global' AND scope_id = 0 AND feature = 'chat'
`, llm.OldImageSectionChatPromptTemplate); err != nil {
		t.Fatalf("set stale default chat prompt: %v", err)
	}

	if err := Apply(ctx, db, nil); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}

	var body string
	if err := db.QueryRowContext(ctx, `
SELECT template_text FROM prompt_templates
WHERE scope_type = 'global' AND scope_id = 0 AND feature = 'chat'
`).Scan(&body); err != nil {
		t.Fatalf("read chat prompt: %v", err)
	}
	if body != llm.DefaultChatPromptTemplate {
		t.Fatalf("expected refreshed default chat prompt")
	}
}

func TestApplyRemovesToneAndTeasingPromptParams(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", "file::memory:?cache=shared")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	for _, migration := range schemaMigrations[:10] {
		if err := applyMigration(ctx, db, migration, nil); err != nil {
			t.Fatalf("apply migration %s: %v", migration.name, err)
		}
	}
	if _, err := db.ExecContext(ctx, `
UPDATE prompt_templates
SET template_text = ?
WHERE scope_type = 'global' AND scope_id = 0 AND feature = 'chat'
`, llm.OldTeasingChatPromptTemplate); err != nil {
		t.Fatalf("set stale default chat prompt: %v", err)
	}

	if err := Apply(ctx, db, nil); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}

	var body string
	if err := db.QueryRowContext(ctx, `
SELECT template_text FROM prompt_templates
WHERE scope_type = 'global' AND scope_id = 0 AND feature = 'chat'
`).Scan(&body); err != nil {
		t.Fatalf("read chat prompt: %v", err)
	}
	if body != llm.DefaultChatPromptTemplate {
		t.Fatalf("expected refreshed default chat prompt")
	}

	for _, table := range []string{"global_settings", "chat_settings"} {
		var remaining int
		if err := db.QueryRowContext(ctx, `
SELECT COUNT(*) FROM pragma_table_info(?) WHERE name IN ('tone_mode', 'allow_teasing')
`, table).Scan(&remaining); err != nil {
			t.Fatalf("inspect %s columns: %v", table, err)
		}
		if remaining != 0 {
			t.Fatalf("expected tone_mode/allow_teasing dropped from %s", table)
		}
	}
}

func TestApplyPreservesCustomChatPrompt(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", "file::memory:?cache=shared")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	for _, migration := range schemaMigrations[:3] {
		if err := applyMigration(ctx, db, migration, nil); err != nil {
			t.Fatalf("apply migration %s: %v", migration.name, err)
		}
	}
	const customPrompt = "custom chat {{.CharacterName}}"
	if _, err := db.ExecContext(ctx, `
UPDATE prompt_templates
SET template_text = ?
WHERE scope_type = 'global' AND scope_id = 0 AND feature = 'chat'
`, customPrompt); err != nil {
		t.Fatalf("set custom chat prompt: %v", err)
	}

	if err := Apply(ctx, db, nil); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}

	var body string
	if err := db.QueryRowContext(ctx, `
SELECT template_text FROM prompt_templates
WHERE scope_type = 'global' AND scope_id = 0 AND feature = 'chat'
`).Scan(&body); err != nil {
		t.Fatalf("read chat prompt: %v", err)
	}
	if body != customPrompt {
		t.Fatalf("expected custom chat prompt to be preserved, got %q", body)
	}
}

func TestApplyUpgradesPreviousDefaultChatPromptWithBotUsername(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", "file::memory:?cache=shared")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	for _, migration := range schemaMigrations[:8] {
		if err := applyMigration(ctx, db, migration, nil); err != nil {
			t.Fatalf("apply migration %s: %v", migration.name, err)
		}
	}
	if _, err := db.ExecContext(ctx, `
UPDATE prompt_templates
SET template_text = ?
WHERE scope_type = 'global' AND scope_id = 0 AND feature = 'chat'
`, llm.PreviousDefaultChatPromptTemplate); err != nil {
		t.Fatalf("set previous default chat prompt: %v", err)
	}

	if err := Apply(ctx, db, nil); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}

	var body string
	if err := db.QueryRowContext(ctx, `
SELECT template_text FROM prompt_templates
WHERE scope_type = 'global' AND scope_id = 0 AND feature = 'chat'
`).Scan(&body); err != nil {
		t.Fatalf("read chat prompt: %v", err)
	}
	if body != llm.DefaultChatPromptTemplate {
		t.Fatalf("expected upgraded default chat prompt")
	}
}

func TestApplyPreservesCustomChatPromptAcrossBotUsernameUpgrade(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", "file::memory:?cache=shared")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	for _, migration := range schemaMigrations[:8] {
		if err := applyMigration(ctx, db, migration, nil); err != nil {
			t.Fatalf("apply migration %s: %v", migration.name, err)
		}
	}
	const customPrompt = "custom chat {{.CharacterName}} {{.BotUsername}}"
	if _, err := db.ExecContext(ctx, `
UPDATE prompt_templates
SET template_text = ?
WHERE scope_type = 'global' AND scope_id = 0 AND feature = 'chat'
`, customPrompt); err != nil {
		t.Fatalf("set custom chat prompt: %v", err)
	}

	if err := Apply(ctx, db, nil); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}

	var body string
	if err := db.QueryRowContext(ctx, `
SELECT template_text FROM prompt_templates
WHERE scope_type = 'global' AND scope_id = 0 AND feature = 'chat'
`).Scan(&body); err != nil {
		t.Fatalf("read chat prompt: %v", err)
	}
	if body != customPrompt {
		t.Fatalf("expected custom chat prompt to be preserved, got %q", body)
	}
}

func TestApplyRenamesChatAliasToCharacterName(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", "file::memory:?cache=shared")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if _, err := db.ExecContext(ctx, `
CREATE TABLE global_settings (
  id INTEGER PRIMARY KEY CHECK (id = 1),
  character_name TEXT NOT NULL,
  language TEXT NOT NULL,
  gender TEXT NOT NULL,
  tone_mode TEXT NOT NULL,
  allow_teasing INTEGER NOT NULL CHECK (allow_teasing IN (0, 1)),
  default_interactivity_mode TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  updated_by INTEGER NOT NULL
);
INSERT INTO global_settings(id, character_name, language, gender, tone_mode, allow_teasing, default_interactivity_mode, updated_at, updated_by)
VALUES(1, 'name', 'English', 'neutral', 'default', 0, 'disabled', CURRENT_TIMESTAMP, 0);
CREATE TABLE chat_settings (
  chat_id INTEGER PRIMARY KEY,
  alias TEXT NOT NULL DEFAULT '',
  language TEXT NOT NULL DEFAULT '',
  gender TEXT NOT NULL DEFAULT '',
  tone_mode TEXT NOT NULL DEFAULT '',
  allow_teasing INTEGER,
  interactivity_mode TEXT NOT NULL DEFAULT '',
  updated_at TEXT NOT NULL,
  updated_by INTEGER NOT NULL
);
INSERT INTO chat_settings(chat_id, alias, language, gender, tone_mode, allow_teasing, interactivity_mode, updated_at, updated_by)
VALUES(123, 'kitsune', 'English', 'female', 'sharp', 1, 'mentions_only', CURRENT_TIMESTAMP, 42);
CREATE TABLE prompt_templates (
  scope_type TEXT NOT NULL,
  scope_id INTEGER NOT NULL,
  feature TEXT NOT NULL,
  template_text TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  updated_by INTEGER NOT NULL,
  PRIMARY KEY (scope_type, scope_id, feature)
);
PRAGMA user_version = 4;
`); err != nil {
		t.Fatalf("prepare old schema: %v", err)
	}

	if err := Apply(ctx, db, nil); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}

	var characterName string
	if err := db.QueryRowContext(ctx, `SELECT character_name FROM chat_settings WHERE chat_id = 123`).Scan(&characterName); err != nil {
		t.Fatalf("read character_name: %v", err)
	}
	if characterName != "kitsune" {
		t.Fatalf("expected migrated character_name, got %q", characterName)
	}

	aliasExists, err := chatSettingsColumnExists(ctx, mustBeginTx(t, db), "alias")
	if err != nil {
		t.Fatalf("check alias column: %v", err)
	}
	if aliasExists {
		t.Fatalf("did not expect old alias column after migration")
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

func TestApplyCreatesStatisticsSnapshotsTable(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", "file::memory:?cache=shared&mode=memory")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if err := Apply(ctx, db, nil); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}

	rows, err := db.QueryContext(ctx, `PRAGMA table_info(statistics_snapshots)`)
	if err != nil {
		t.Fatalf("table_info: %v", err)
	}
	defer func() { _ = rows.Close() }()
	columns := map[string]bool{}
	for rows.Next() {
		var cid, notNull, pk int
		var name, typ string
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &typ, &notNull, &dflt, &pk); err != nil {
			t.Fatalf("scan: %v", err)
		}
		columns[name] = true
	}
	for _, want := range []string{"dataset", "format_version", "generation", "revision", "payload", "saved_at"} {
		if !columns[want] {
			t.Fatalf("statistics_snapshots is missing column %q (have %v)", want, columns)
		}
	}

	var version int
	if err := db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil || version != targetSchemaVersion {
		t.Fatalf("expected schema version %d, got %d (err %v)", targetSchemaVersion, version, err)
	}
}

func openStickerMigrationDB(t *testing.T) *sql.DB {
	t.Helper()

	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "bot.sqlite"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`PRAGMA foreign_keys=ON`); err != nil {
		t.Fatalf("enable foreign keys: %v", err)
	}

	return db
}

func TestApplyCreatesStickerCatalogOnFreshAndV13Databases(t *testing.T) {
	ctx := context.Background()

	for name, prepare := range map[string]func(*sql.DB){
		"fresh": func(*sql.DB) {},
		"from v13": func(db *sql.DB) {
			if err := Apply(ctx, db, nil); err != nil {
				t.Fatalf("seed apply: %v", err)
			}
			for _, stmt := range []string{
				`DROP TABLE sticker_entries`,
				`DROP TABLE sticker_sets`,
				`PRAGMA user_version = 13`,
			} {
				if _, err := db.ExecContext(ctx, stmt); err != nil {
					t.Fatalf("downgrade to v13 (%s): %v", stmt, err)
				}
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			db := openStickerMigrationDB(t)
			prepare(db)

			if err := Apply(ctx, db, nil); err != nil {
				t.Fatalf("apply migrations: %v", err)
			}

			var version int
			if err := db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil || version != targetSchemaVersion {
				t.Fatalf("expected schema version %d, got %d (err %v)", targetSchemaVersion, version, err)
			}

			now := "2026-11-21T00:00:00Z"
			if _, err := db.ExecContext(ctx, `INSERT INTO sticker_sets (name, title, synced_at, created_at, updated_at) VALUES ('s', 'S', ?, ?, ?)`, now, now, now); err != nil {
				t.Fatalf("insert set: %v", err)
			}
			var revision int
			if err := db.QueryRowContext(ctx, `SELECT revision FROM sticker_sets WHERE name = 's'`).Scan(&revision); err != nil || revision != 1 {
				t.Fatalf("expected default revision 1, got %d (err %v)", revision, err)
			}

			insert := func(unique string, semantic any) error {
				_, err := db.ExecContext(ctx, `INSERT INTO sticker_entries
(set_name, file_unique_id, file_id, semantic_id, format, position, available, synced_at, updated_at)
VALUES ('s', ?, 'f', ?, 'static', 1, 1, ?, ?)`, unique, semantic, now, now)

				return err
			}

			// Several unannotated rows coexist: the unique index covers assigned IDs only.
			for _, unique := range []string{"u1", "u2", "u3"} {
				if err := insert(unique, nil); err != nil {
					t.Fatalf("insert unannotated %s: %v", unique, err)
				}
			}
			if err := insert("u4", "kitsu_happy"); err != nil {
				t.Fatalf("insert annotated: %v", err)
			}
			if err := insert("u5", "kitsu_happy"); err == nil {
				t.Fatalf("expected duplicate semantic_id to violate the unique index")
			}
			if err := insert("u6", ""); err == nil {
				t.Fatalf("expected an empty semantic_id to violate the CHECK constraint")
			}

			if _, err := db.ExecContext(ctx, `DELETE FROM sticker_sets WHERE name = 's'`); err != nil {
				t.Fatalf("delete set: %v", err)
			}
			var remaining int
			if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sticker_entries`).Scan(&remaining); err != nil || remaining != 0 {
				t.Fatalf("expected entries to cascade with the set, got %d (err %v)", remaining, err)
			}
		})
	}
}
