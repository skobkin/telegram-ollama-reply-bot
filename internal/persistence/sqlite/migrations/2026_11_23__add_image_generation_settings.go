package migrations

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
)

// migrateV16AddImageGenerationSettings introduces the runtime half of image
// generation (#189): a global permission that is ON (the feature is already
// opt-in through the IMAGE_GENERATION__* environment section, so the switch
// only gates a backend an operator configured), a nullable per-chat override
// (NULL = inherit), and the global generation model name. The model defaults
// to the empty string on purpose: there is no environment default for it
// (#139), and an empty model keeps the tool unavailable until an operator
// picks one.
func migrateV16AddImageGenerationSettings(ctx context.Context, tx *sql.Tx, log *slog.Logger) error {
	type columnAddition struct {
		table  string
		column string
		ddl    string
		exists func(context.Context, *sql.Tx, string) (bool, error)
	}

	additions := []columnAddition{
		{
			table:  "global_settings",
			column: "image_generation_enabled",
			ddl:    `ALTER TABLE global_settings ADD COLUMN image_generation_enabled INTEGER NOT NULL DEFAULT 1`,
			exists: globalSettingsColumnExists,
		},
		{
			table:  "global_settings",
			column: "image_generation_model",
			ddl:    `ALTER TABLE global_settings ADD COLUMN image_generation_model TEXT NOT NULL DEFAULT ''`,
			exists: globalSettingsColumnExists,
		},
		{
			table:  "chat_settings",
			column: "image_generation_enabled",
			ddl:    `ALTER TABLE chat_settings ADD COLUMN image_generation_enabled INTEGER`,
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
