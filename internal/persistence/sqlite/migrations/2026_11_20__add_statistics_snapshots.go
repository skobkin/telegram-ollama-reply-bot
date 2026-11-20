package migrations

import (
	"context"
	"database/sql"
	"log/slog"
)

// migrateV13AddStatisticsSnapshots creates the aggregate statistics snapshot
// table. Each persistent statistics dataset owns exactly one row that is
// replaced whole on every save: no per-event rows and no time buckets. The
// dataset column is validated in Go ('interaction', 'tool_usage', or a
// '<dataset>.unreadable' quarantine slot).
func migrateV13AddStatisticsSnapshots(ctx context.Context, tx *sql.Tx, log *slog.Logger) error {
	return applyStatements(ctx, tx, "add_statistics_snapshots", []string{
		`CREATE TABLE IF NOT EXISTS statistics_snapshots (
  dataset TEXT PRIMARY KEY,
  format_version INTEGER NOT NULL,
  generation INTEGER NOT NULL,
  revision INTEGER NOT NULL,
  payload BLOB NOT NULL,
  saved_at TEXT NOT NULL
);`,
	}, log)
}
