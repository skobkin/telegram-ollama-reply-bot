package mcp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"telegram-ollama-reply-bot/internal/config"
	"telegram-ollama-reply-bot/internal/tooluse"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// implementation is the bot's MCP client identity. It surfaces in any
// server-side log that records the calling client.
var implementation = &mcp.Implementation{
	Name:    "telegram-ollama-reply-bot",
	Version: "0.0.0",
}

// Manager owns the open MCP sessions for every configured server. It is the
// only thing the application layer needs to talk to: build a Manager from
// config, ask it for the translated toolset, register it on the tool
// registry, and Close it on shutdown.
type Manager struct {
	clients []*client
	logger  *slog.Logger
}

// NewManager wires the bot to every server in `cfg.Servers`, in order. A
// misconfigured server is a hard error unless it is marked `Optional` — in
// that case the manager logs a warning, drops the server, and keeps going
// with the survivors. The error return is reserved for completely
// unrecoverable situations (e.g. a config-shaped problem that prevents
// even attempting to connect).
func NewManager(ctx context.Context, cfg config.MCPConfig, logger *slog.Logger) (*Manager, error) {
	if logger == nil {
		logger = slog.Default()
	}
	if !cfg.Enabled {
		logger.Info("mcp support disabled by config")

		return &Manager{logger: logger}, nil
	}
	if len(cfg.Servers) == 0 {
		logger.Info("mcp support enabled but no servers configured")

		return &Manager{logger: logger}, nil
	}

	manager := &Manager{logger: logger}
	for name, server := range cfg.Servers {
		raw := mcp.NewClient(implementation, nil)
		cl := &client{
			name:   name,
			config: server,
			raw:    raw,
			logger: serverLogger(logger, name),
		}
		connectCtx, cancel := context.WithTimeout(ctx, connectTimeout(server))
		err := cl.connect(connectCtx)
		cancel()
		if err != nil {
			if server.Optional {
				logger.Warn(
					"mcp server optional, skipping",
					"server", name,
					"error", err,
				)

				continue
			}
			_ = manager.Close()

			return nil, fmt.Errorf("mcp server %q: %w", name, err)
		}
		manager.clients = append(manager.clients, cl)
		logger.Info(
			"mcp server connected",
			"server", name,
			"tool_count", len(cl.definitions),
		)
		for _, def := range cl.definitions {
			logTranslatedDefinition(logger, name, namespacedNameFromDefinition(def), def.Name, def)
		}
	}

	return manager, nil
}

// connectTimeout is the deadline for a single server's Connect + ListTools
// handshake. It is derived from the per-server timeout, with a sensible
// minimum so a half-second configured value still has time to authenticate.
func connectTimeout(server config.MCPServerConfig) time.Duration {
	if server.Timeout > 0 {
		// 4x the per-request timeout gives the handshake room to complete
		// (initialize + initialized + tools/list) without a separate config knob.
		multiplied := server.Timeout * 4
		if multiplied < 5*time.Second {
			return 5 * time.Second
		}

		return multiplied
	}

	return 4 * defaultClientTimeout
}

// Definitions returns every translated tool from every server. The slice is
// safe to iterate; the underlying Handler closures are independent of this
// slice and can be invoked concurrently from the tooluse runtime.
func (m *Manager) Definitions() []tooluse.Definition {
	if m == nil || len(m.clients) == 0 {
		return nil
	}
	out := make([]tooluse.Definition, 0)
	for _, cl := range m.clients {
		out = append(out, cl.definitions...)
	}

	return out
}

// Close tears down every open session. Safe to call multiple times. Errors
// are aggregated and returned as a joined error so the caller can log them
// without losing any individual cause.
func (m *Manager) Close() error {
	if m == nil || len(m.clients) == 0 {
		return nil
	}
	var errs []error
	for _, cl := range m.clients {
		if err := cl.close(); err != nil {
			errs = append(errs, fmt.Errorf("close %s: %w", cl.name, err))
		}
	}
	m.clients = nil

	if len(errs) == 0 {
		return nil
	}

	return errors.Join(errs...)
}

// namespacedNameFromDefinition extracts the namespaced name we assigned at
// translation time. Kept as a helper to avoid leaking the implementation
// detail that Definition.Name carries the namespaced form for MCP tools.
func namespacedNameFromDefinition(def tooluse.Definition) string {
	return def.Name
}
