package migrations

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
)

// migrateV6AddAccessPolicy introduces the explicit access policy column on
// global_settings. The column defaults to 'allowlist', which deterministically
// migrates both legacy shapes: installs with a populated whitelist keep their
// exact behavior, and installs with an empty whitelist change from the old
// implicit "allow everyone" to "deny ordinary chats" — a documented breaking
// change (see UPGRADE.md). No install is ever migrated to allow_all; that
// stays an explicit operator decision.
func migrateV6AddAccessPolicy(ctx context.Context, tx *sql.Tx, log *slog.Logger) error {
	exists, err := globalSettingsColumnExists(ctx, tx, "access_policy")
	if err != nil {
		return err
	}
	if exists {
		if log != nil {
			log.Debug("sqlite migration column already exists", "table", "global_settings", "column", "access_policy")
		}

		return nil
	}

	if log != nil {
		log.Debug("adding sqlite migration column", "table", "global_settings", "column", "access_policy", "default", "allowlist")
	}
	if _, err := tx.ExecContext(ctx, `ALTER TABLE global_settings ADD COLUMN access_policy TEXT NOT NULL DEFAULT 'allowlist'`); err != nil {
		return fmt.Errorf("add access_policy column: %w", err)
	}

	return nil
}

func globalSettingsColumnExists(ctx context.Context, tx *sql.Tx, columnName string) (bool, error) {
	rows, err := tx.QueryContext(ctx, `PRAGMA table_info(global_settings)`)
	if err != nil {
		return false, err
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var cid int
		var name string
		var typ string
		var notNull int
		var defaultValue sql.NullString
		var pk int
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
			return false, err
		}
		if name == columnName {
			return true, nil
		}
	}

	return false, rows.Err()
}
