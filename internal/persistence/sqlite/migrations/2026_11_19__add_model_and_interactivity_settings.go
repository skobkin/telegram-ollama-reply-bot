package migrations

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
)

// migrateV12AddModelAndInteractivitySettings moves model selection and the
// interaction profile into SQLite (issue #128 and the System One decision
// model). Global defaults: chat/summarize/image-recognition/system-one model
// overrides (” = environment routes stay in effect), the default
// interaction profile ('non_interactive' — proactive participation stays
// off everywhere until an operator opts in), and decision instructions
// (” = unset). Per chat: ” overrides for the generation and decision
// models, the interaction profile, and the decision instructions — every
// new chat column defaults to ”, meaning inherit, so the migration
// preserves existing behavior unchanged.
func migrateV12AddModelAndInteractivitySettings(ctx context.Context, tx *sql.Tx, log *slog.Logger) error {
	type columnAddition struct {
		table  string
		column string
		ddl    string
	}

	additions := []columnAddition{
		{table: "global_settings", column: "chat_model", ddl: `ALTER TABLE global_settings ADD COLUMN chat_model TEXT NOT NULL DEFAULT ''`},
		{table: "global_settings", column: "summarize_model", ddl: `ALTER TABLE global_settings ADD COLUMN summarize_model TEXT NOT NULL DEFAULT ''`},
		{table: "global_settings", column: "image_recognition_model", ddl: `ALTER TABLE global_settings ADD COLUMN image_recognition_model TEXT NOT NULL DEFAULT ''`},
		{table: "global_settings", column: "systemone_model", ddl: `ALTER TABLE global_settings ADD COLUMN systemone_model TEXT NOT NULL DEFAULT ''`},
		{table: "global_settings", column: "default_interaction_profile", ddl: `ALTER TABLE global_settings ADD COLUMN default_interaction_profile TEXT NOT NULL DEFAULT 'non_interactive'`},
		{table: "global_settings", column: "decision_instructions", ddl: `ALTER TABLE global_settings ADD COLUMN decision_instructions TEXT NOT NULL DEFAULT ''`},
		{table: "chat_settings", column: "chat_model", ddl: `ALTER TABLE chat_settings ADD COLUMN chat_model TEXT NOT NULL DEFAULT ''`},
		{table: "chat_settings", column: "systemone_model", ddl: `ALTER TABLE chat_settings ADD COLUMN systemone_model TEXT NOT NULL DEFAULT ''`},
		{table: "chat_settings", column: "interaction_profile", ddl: `ALTER TABLE chat_settings ADD COLUMN interaction_profile TEXT NOT NULL DEFAULT ''`},
		{table: "chat_settings", column: "decision_instructions", ddl: `ALTER TABLE chat_settings ADD COLUMN decision_instructions TEXT NOT NULL DEFAULT ''`},
	}

	for _, addition := range additions {
		var exists bool
		var err error
		switch addition.table {
		case "global_settings":
			exists, err = globalSettingsColumnExists(ctx, tx, addition.column)
		case "chat_settings":
			exists, err = chatSettingsColumnExists(ctx, tx, addition.column)
		default:
			err = fmt.Errorf("unknown table %q", addition.table)
		}
		if err != nil {
			return fmt.Errorf("check %s.%s: %w", addition.table, addition.column, err)
		}
		if exists {
			if log != nil {
				log.Debug("sqlite migration column already exists", "table", addition.table, "column", addition.column)
			}

			continue
		}
		if log != nil {
			log.Debug("adding sqlite migration column", "table", addition.table, "column", addition.column)
		}
		if _, err := tx.ExecContext(ctx, addition.ddl); err != nil {
			return fmt.Errorf("add %s.%s: %w", addition.table, addition.column, err)
		}
	}

	return nil
}
