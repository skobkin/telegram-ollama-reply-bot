package migrations

import (
	"context"
	"database/sql"
	"log/slog"
)

func migrateV3AddReminders(ctx context.Context, tx *sql.Tx, log *slog.Logger) error {
	return applyStatements(ctx, tx, "add_reminders", []string{
		`CREATE TABLE IF NOT EXISTS reminders (
  id TEXT PRIMARY KEY,
  chat_id INTEGER NOT NULL,
  topic_id INTEGER NOT NULL DEFAULT 0,
  creator_user_id INTEGER NOT NULL,
  text TEXT NOT NULL,
  schedule_type TEXT NOT NULL,
  timezone TEXT NOT NULL DEFAULT '',
  next_due_at TEXT NOT NULL,
  last_delivered_at TEXT NOT NULL DEFAULT '',
  interval_value INTEGER NOT NULL DEFAULT 0,
  weekday_mask INTEGER NOT NULL DEFAULT 0,
  day_of_month INTEGER NOT NULL DEFAULT 0,
  time_of_day_minutes INTEGER NOT NULL DEFAULT 0,
  is_active INTEGER NOT NULL CHECK (is_active IN (0, 1)),
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);`,
		`CREATE INDEX IF NOT EXISTS idx_reminders_active_due ON reminders(is_active, next_due_at);`,
		`CREATE INDEX IF NOT EXISTS idx_reminders_chat_active ON reminders(chat_id, is_active, next_due_at);`,
	}, log)
}
