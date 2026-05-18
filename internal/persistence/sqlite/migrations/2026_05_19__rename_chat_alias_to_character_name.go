package migrations

import (
	"context"
	"database/sql"
	"log/slog"
)

func migrateV5RenameChatAliasToCharacterName(ctx context.Context, tx *sql.Tx, log *slog.Logger) error {
	hasCharacterName, err := chatSettingsColumnExists(ctx, tx, "character_name")
	if err != nil {
		return err
	}
	if hasCharacterName {
		if log != nil {
			log.Debug("sqlite migration column already exists", "table", "chat_settings", "column", "character_name")
		}

		return nil
	}

	hasAlias, err := chatSettingsColumnExists(ctx, tx, "alias")
	if err != nil {
		return err
	}
	if !hasAlias {
		return nil
	}

	if log != nil {
		log.Debug("renaming sqlite migration column", "table", "chat_settings", "from", "alias", "to", "character_name")
	}
	if _, err := tx.ExecContext(ctx, `ALTER TABLE chat_settings RENAME COLUMN alias TO character_name`); err != nil {
		return err
	}

	return nil
}
