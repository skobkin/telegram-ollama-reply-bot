package migrations

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
)

const targetSchemaVersion = 2

type migrationStep struct {
	version int
	name    string
	apply   func(context.Context, *sql.Tx, *slog.Logger) error
}

var schemaMigrations = []migrationStep{
	{version: 1, name: "bootstrap_admin_config", apply: migrateV1BootstrapAdminConfig},
	{version: 2, name: "add_chat_language_and_gender", apply: migrateV2AddChatLanguageAndGender},
}

func Apply(ctx context.Context, db *sql.DB, log *slog.Logger) error {
	if log == nil {
		log = slog.Default()
	}

	version, err := readSchemaVersion(ctx, db)
	if err != nil {
		return err
	}
	log.Debug("loaded sqlite schema version", "current_version", version, "target_version", targetSchemaVersion)

	if version >= targetSchemaVersion {
		log.Debug("sqlite schema already up to date", "current_version", version)

		return nil
	}

	for _, migration := range schemaMigrations {
		if version >= migration.version {
			continue
		}
		migrationLog := log.With("version", migration.version, "name", migration.name)
		migrationLog.Info("applying sqlite migration")
		if err := applyMigration(ctx, db, migration, migrationLog); err != nil {
			return fmt.Errorf("apply migration %s (%d): %w", migration.name, migration.version, err)
		}
		migrationLog.Info("sqlite migration applied")
		version = migration.version
	}

	return nil
}

func applyMigration(ctx context.Context, db *sql.DB, migration migrationStep, log *slog.Logger) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin migration tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if log != nil {
		log.Debug("started sqlite migration transaction")
	}
	if err := migration.apply(ctx, tx, log); err != nil {
		return err
	}
	if err := setSchemaVersionTx(ctx, tx, migration.version); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit migration tx: %w", err)
	}
	if log != nil {
		log.Debug("committed sqlite migration transaction")
	}

	return nil
}

func readSchemaVersion(ctx context.Context, db *sql.DB) (int, error) {
	var version int
	if err := db.QueryRowContext(ctx, `PRAGMA user_version;`).Scan(&version); err != nil {
		return 0, fmt.Errorf("read schema version: %w", err)
	}

	return version, nil
}

func setSchemaVersionTx(ctx context.Context, tx *sql.Tx, version int) error {
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`PRAGMA user_version = %d;`, version)); err != nil {
		return fmt.Errorf("set schema version %d: %w", version, err)
	}

	return nil
}

func applyStatements(ctx context.Context, tx *sql.Tx, migrationName string, statements []string, log *slog.Logger) error {
	for i, stmt := range statements {
		if log != nil {
			log.Debug("executing sqlite migration statement", "migration", migrationName, "statement_index", i+1, "statement_count", len(statements))
		}
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("apply %s statement: %w", migrationName, err)
		}
	}

	return nil
}
