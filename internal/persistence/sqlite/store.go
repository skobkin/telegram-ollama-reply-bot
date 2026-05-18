package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"telegram-ollama-reply-bot/internal/persistence/sqlite/migrations"

	// Register the SQLite driver for database/sql.
	_ "modernc.org/sqlite"
)

type Store struct {
	db *sql.DB
}

func Open(ctx context.Context, path string, logger *slog.Logger) (*Store, error) {
	if path == "" {
		return nil, errors.New("persistent store path is empty")
	}
	if logger == nil {
		logger = slog.Default()
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("create persistent store directory: %w", err)
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite store: %w", err)
	}
	// SQLite has a single writer. Keep database/sql from opening extra
	// connections without the connection-scoped PRAGMAs below and serialize
	// in-process writes instead of surfacing SQLITE_BUSY during update bursts.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	for _, stmt := range []string{
		`PRAGMA foreign_keys = ON;`,
		`PRAGMA journal_mode = WAL;`,
		`PRAGMA busy_timeout = 5000;`,
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			_ = db.Close()

			return nil, fmt.Errorf("configure sqlite: %w", err)
		}
	}

	if err := migrations.Apply(ctx, db, logger.With("component", "migrations")); err != nil {
		_ = db.Close()

		return nil, err
	}

	return &Store{db: db}, nil
}

func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}

	return s.db.Close()
}
