package migrations

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
)

const targetSchemaVersion = 1

type migrationStep struct {
	version int
	name    string
	apply   func(context.Context, *sql.Tx) error
}

var schemaMigrations = []migrationStep{
	{version: 1, name: "bootstrap_admin_config", apply: migrateV1BootstrapAdminConfig},
}

func Apply(ctx context.Context, db *sql.DB, log *slog.Logger) error {
	version, err := readSchemaVersion(ctx, db)
	if err != nil {
		return err
	}

	if version >= targetSchemaVersion {
		return nil
	}

	for _, migration := range schemaMigrations {
		if version >= migration.version {
			continue
		}
		if log != nil {
			log.Info("applying sqlite migration", "version", migration.version, "name", migration.name)
		}
		if err := applyMigration(ctx, db, migration); err != nil {
			return fmt.Errorf("apply migration %s (%d): %w", migration.name, migration.version, err)
		}
		version = migration.version
	}

	return nil
}

func applyMigration(ctx context.Context, db *sql.DB, migration migrationStep) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin migration tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := migration.apply(ctx, tx); err != nil {
		return err
	}
	if err := setSchemaVersionTx(ctx, tx, migration.version); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit migration tx: %w", err)
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

func applyStatements(ctx context.Context, tx *sql.Tx, migrationName string, statements []string) error {
	for _, stmt := range statements {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("apply %s statement: %w", migrationName, err)
		}
	}

	return nil
}
