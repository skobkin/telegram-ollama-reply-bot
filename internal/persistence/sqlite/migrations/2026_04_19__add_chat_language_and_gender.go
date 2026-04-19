package migrations

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
)

func migrateV2AddChatLanguageAndGender(ctx context.Context, tx *sql.Tx, log *slog.Logger) error {
	for _, column := range []string{"language", "gender"} {
		exists, err := columnExists(ctx, tx, "chat_settings", column)
		if err != nil {
			return fmt.Errorf("check chat_settings.%s: %w", column, err)
		}
		if exists {
			if log != nil {
				log.Debug("sqlite migration column already exists", "table", "chat_settings", "column", column)
			}

			continue
		}

		if log != nil {
			log.Debug("adding sqlite migration column", "table", "chat_settings", "column", column)
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`ALTER TABLE chat_settings ADD COLUMN %s TEXT NOT NULL DEFAULT ''`, column)); err != nil {
			return fmt.Errorf("add chat_settings.%s: %w", column, err)
		}
	}

	return nil
}

func columnExists(ctx context.Context, tx *sql.Tx, tableName, columnName string) (bool, error) {
	rows, err := tx.QueryContext(ctx, fmt.Sprintf(`PRAGMA table_info(%s)`, tableName))
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
