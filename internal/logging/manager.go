package logging

import (
	"log/slog"
	"sync"

	"telegram-ollama-reply-bot/internal/support/secretscrub"
)

// Manager owns process logger configuration and scoped child logger creation.
type Manager struct {
	mu       sync.RWMutex
	logger   *slog.Logger
	scrubber *secretscrub.Scrubber
}

// NewManager creates a manager with sane defaults.
func NewManager(opts Options) (*Manager, error) {
	m := &Manager{scrubber: opts.Scrubber}
	if err := m.Configure(opts); err != nil {
		return nil, err
	}

	return m, nil
}

// Configure applies runtime logger settings.
func (m *Manager) Configure(opts Options) error {
	logger, err := buildLogger(opts, m.scrubber)
	if err != nil {
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	m.logger = logger
	if opts.SetDefault {
		slog.SetDefault(logger)
	}

	return nil
}

// Logger returns a component-scoped logger with package attribute set.
func (m *Manager) Logger(pkg string) *slog.Logger {
	m.mu.RLock()
	defer m.mu.RUnlock()

	return m.logger.With("pkg", pkg)
}
