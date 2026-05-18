package logging

import (
	"log/slog"
	"sync"
)

// Manager owns process logger configuration and scoped child logger creation.
type Manager struct {
	mu     sync.RWMutex
	logger *slog.Logger
}

// NewManager creates a manager with sane defaults.
func NewManager(opts Options) (*Manager, error) {
	m := &Manager{}
	if err := m.Configure(opts); err != nil {
		return nil, err
	}

	return m, nil
}

// Configure applies runtime logger settings.
func (m *Manager) Configure(opts Options) error {
	logger, err := buildLogger(opts)
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
