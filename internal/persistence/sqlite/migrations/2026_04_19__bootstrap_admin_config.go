package migrations

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"

	"telegram-ollama-reply-bot/internal/llm"
)

func migrateV1BootstrapAdminConfig(ctx context.Context, tx *sql.Tx, log *slog.Logger) error {
	if err := applyStatements(ctx, tx, "bootstrap_admin_config", []string{
		`CREATE TABLE IF NOT EXISTS global_settings (
  id INTEGER PRIMARY KEY CHECK (id = 1),
  character_name TEXT NOT NULL,
  language TEXT NOT NULL,
  gender TEXT NOT NULL,
  tone_mode TEXT NOT NULL,
  allow_teasing INTEGER NOT NULL CHECK (allow_teasing IN (0, 1)),
  default_interactivity_mode TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  updated_by INTEGER NOT NULL
);`,
		`CREATE TABLE IF NOT EXISTS chat_settings (
  chat_id INTEGER PRIMARY KEY,
  character_name TEXT NOT NULL DEFAULT '',
  language TEXT NOT NULL DEFAULT '',
  gender TEXT NOT NULL DEFAULT '',
  tone_mode TEXT NOT NULL DEFAULT '',
  allow_teasing INTEGER,
  interactivity_mode TEXT NOT NULL DEFAULT '',
  updated_at TEXT NOT NULL,
  updated_by INTEGER NOT NULL
);`,
		`CREATE TABLE IF NOT EXISTS prompt_templates (
  scope_type TEXT NOT NULL,
  scope_id INTEGER NOT NULL,
  feature TEXT NOT NULL,
  template_text TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  updated_by INTEGER NOT NULL,
  PRIMARY KEY (scope_type, scope_id, feature)
);`,
		`CREATE TABLE IF NOT EXISTS chat_catalog (
  chat_id INTEGER PRIMARY KEY,
  chat_type TEXT NOT NULL,
  title TEXT NOT NULL DEFAULT '',
  username TEXT NOT NULL DEFAULT '',
  display_name TEXT NOT NULL DEFAULT '',
  first_seen_at TEXT NOT NULL,
  last_seen_at TEXT NOT NULL
);`,
		`CREATE TABLE IF NOT EXISTS chat_whitelist (
  chat_id INTEGER PRIMARY KEY,
  added_at TEXT NOT NULL,
  added_by INTEGER NOT NULL
);`,
		`CREATE INDEX IF NOT EXISTS idx_chat_catalog_last_seen_at ON chat_catalog(last_seen_at DESC);`,
	}, log); err != nil {
		return err
	}

	if _, err := tx.ExecContext(ctx, `
INSERT INTO global_settings(
  id, character_name, language, gender, tone_mode, allow_teasing, default_interactivity_mode, updated_at, updated_by
)
VALUES(1, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP, 0)
ON CONFLICT(id) DO NOTHING
`, llm.DefaultCharacterName, llm.DefaultLanguage, llm.DefaultGender, llm.DefaultToneMode, boolToInt(llm.DefaultAllowTeasing), "disabled"); err != nil {
		return fmt.Errorf("seed global settings: %w", err)
	}

	for _, feature := range []struct {
		name string
		body string
	}{
		{name: "chat", body: llm.DefaultChatPromptTemplate},
		{name: "summarize", body: llm.DefaultSummarizePromptTemplate},
		{name: "image_recognition", body: llm.DefaultImageRecognitionPromptTemplate},
	} {
		if _, err := tx.ExecContext(ctx, `
INSERT INTO prompt_templates(scope_type, scope_id, feature, template_text, updated_at, updated_by)
VALUES('global', 0, ?, ?, CURRENT_TIMESTAMP, 0)
ON CONFLICT(scope_type, scope_id, feature) DO NOTHING
`, feature.name, feature.body); err != nil {
			return fmt.Errorf("seed %s prompt template: %w", feature.name, err)
		}
	}

	return nil
}

func boolToInt(v bool) int {
	if v {
		return 1
	}

	return 0
}
