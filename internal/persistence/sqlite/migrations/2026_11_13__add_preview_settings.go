package migrations

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
)

// migrateV8AddPreviewSettings introduces the live reply preview settings:
// a global master switch (off — backward-compatible rollout), a global
// default preview mode ('tools'), global tool-history retention (on), a
// nullable per-chat preview mode (” = inherit), and a nullable per-chat
// tool-history toggle (NULL = inherit). Previews stay disabled everywhere
// after migration; enabling them is an explicit operator decision.
func migrateV8AddPreviewSettings(ctx context.Context, tx *sql.Tx, log *slog.Logger) error {
	type columnAddition struct {
		table  string
		column string
		ddl    string
	}

	additions := []columnAddition{
		{table: "global_settings", column: "show_reply_previews", ddl: `ALTER TABLE global_settings ADD COLUMN show_reply_previews INTEGER NOT NULL DEFAULT 0`},
		{table: "global_settings", column: "default_preview_mode", ddl: `ALTER TABLE global_settings ADD COLUMN default_preview_mode TEXT NOT NULL DEFAULT 'tools'`},
		{table: "global_settings", column: "keep_tool_history", ddl: `ALTER TABLE global_settings ADD COLUMN keep_tool_history INTEGER NOT NULL DEFAULT 1`},
		{table: "chat_settings", column: "preview_mode", ddl: `ALTER TABLE chat_settings ADD COLUMN preview_mode TEXT NOT NULL DEFAULT ''`},
		{table: "chat_settings", column: "keep_tool_history", ddl: `ALTER TABLE chat_settings ADD COLUMN keep_tool_history INTEGER`},
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
