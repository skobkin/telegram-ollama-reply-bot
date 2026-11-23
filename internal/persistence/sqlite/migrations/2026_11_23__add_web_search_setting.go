package migrations

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
)

// migrateV15AddWebSearchSetting introduces the web search permission: a
// global default that is ON (existing deployments keep their current
// behavior) and a nullable per-chat override (NULL = inherit). The setting
// only gates a search backend that is already configured in the environment;
// it never creates a search capability.
func migrateV15AddWebSearchSetting(ctx context.Context, tx *sql.Tx, log *slog.Logger) error {
	type columnAddition struct {
		table  string
		column string
		ddl    string
		exists func(context.Context, *sql.Tx, string) (bool, error)
	}

	additions := []columnAddition{
		{
			table:  "global_settings",
			column: "web_search_enabled",
			ddl:    `ALTER TABLE global_settings ADD COLUMN web_search_enabled INTEGER NOT NULL DEFAULT 1`,
			exists: globalSettingsColumnExists,
		},
		{
			table:  "chat_settings",
			column: "web_search_enabled",
			ddl:    `ALTER TABLE chat_settings ADD COLUMN web_search_enabled INTEGER`,
			exists: chatSettingsColumnExists,
		},
	}

	for _, addition := range additions {
		exists, err := addition.exists(ctx, tx, addition.column)
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
