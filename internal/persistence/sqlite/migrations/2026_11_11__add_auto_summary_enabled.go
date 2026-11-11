package migrations

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
)

// migrateV7AddAutoSummaryEnabled introduces the auto-summarizer toggle
// (issue #125): a nullable chat_settings column (NULL = inherit the global
// default) and a global default column fixed at 0. The feature stays off
// everywhere after migration; enabling it is an explicit operator decision.
func migrateV7AddAutoSummaryEnabled(ctx context.Context, tx *sql.Tx, log *slog.Logger) error {
	chatExists, err := chatSettingsColumnExists(ctx, tx, "auto_summary_enabled")
	if err != nil {
		return fmt.Errorf("check chat_settings.auto_summary_enabled: %w", err)
	}
	if chatExists {
		if log != nil {
			log.Debug("sqlite migration column already exists", "table", "chat_settings", "column", "auto_summary_enabled")
		}
	} else {
		if log != nil {
			log.Debug("adding sqlite migration column", "table", "chat_settings", "column", "auto_summary_enabled")
		}
		if _, err := tx.ExecContext(ctx, `ALTER TABLE chat_settings ADD COLUMN auto_summary_enabled INTEGER`); err != nil {
			return fmt.Errorf("add chat_settings.auto_summary_enabled: %w", err)
		}
	}

	globalExists, err := globalSettingsColumnExists(ctx, tx, "default_auto_summary_enabled")
	if err != nil {
		return fmt.Errorf("check global_settings.default_auto_summary_enabled: %w", err)
	}
	if globalExists {
		if log != nil {
			log.Debug("sqlite migration column already exists", "table", "global_settings", "column", "default_auto_summary_enabled")
		}

		return nil
	}

	if log != nil {
		log.Debug("adding sqlite migration column", "table", "global_settings", "column", "default_auto_summary_enabled", "default", 0)
	}
	if _, err := tx.ExecContext(ctx, `ALTER TABLE global_settings ADD COLUMN default_auto_summary_enabled INTEGER NOT NULL DEFAULT 0`); err != nil {
		return fmt.Errorf("add global_settings.default_auto_summary_enabled: %w", err)
	}

	return nil
}
