package mcp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
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

// NewManager wires the bot to every server in `cfg.Servers`, in sorted-name
// order: `cfg.Servers` is a Go map with a randomized iteration order, so
// sorting pins the registration order and keeps the resulting tool list
// (and therefore the LLM prompt) stable across restarts. The handshakes run
// concurrently — each server gets its full connect-timeout budget and slow or
// dead servers do not add their budgets to boot time — while registration,
// duplicate detection, and logging stay strictly in sorted order so the
// outcome (and log stream) is deterministic across restarts. A misconfigured
// server is a hard error unless it is marked `Optional` — in that case the
// manager logs a warning, drops the server, and keeps going with the
// survivors. The error return is reserved for completely unrecoverable
// situations (e.g. a config-shaped problem that prevents even attempting to
// connect).
func NewManager(ctx context.Context, cfg config.MCPConfig, logger *slog.Logger) (*Manager, error) {
	if logger == nil {
		logger = slog.Default()
	}
	if !cfg.Enabled {
		// Surface the misconfiguration when the operator configured servers
		// but forgot to enable MCP: a plain "disabled" INFO would hide it.
		// Server NAMES only — URLs may carry credentials in headers and are
		// never logged.
		if len(cfg.Servers) > 0 {
			names := sortedServerNames(cfg.Servers)
			logger.Warn(
				"mcp support disabled by config while servers are configured",
				"servers", names,
				"count", len(names),
			)

			return &Manager{logger: logger}, nil
		}
		logger.Info("mcp support disabled by config")

		return &Manager{logger: logger}, nil
	}
	if len(cfg.Servers) == 0 {
		logger.Info("mcp support enabled but no servers configured")

		return &Manager{logger: logger}, nil
	}

	names := sortedServerNames(cfg.Servers)

	// Handshake phase: one goroutine per server, each writing only its own
	// slot. Every client owns its session and HTTP client, slog is
	// concurrency-safe, and no Manager field is touched — the workers only
	// produce results, all shared state (and all logging) happens in the
	// sorted pass below.
	type connectResult struct {
		client *client
		err    error
	}
	results := make([]connectResult, len(names))
	var wg sync.WaitGroup
	for i, name := range names {
		wg.Add(1)
		go func() {
			defer wg.Done()
			server := cfg.Servers[name]
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
			results[i] = connectResult{client: cl, err: err}
		}()
	}
	wg.Wait()

	manager := &Manager{logger: logger}
	// namespacedName -> server that produced it. A duplicate means two
	// different (server, tool) pairs collapse onto one final namespaced name;
	// the later registration would silently replace the earlier one on the
	// registry (and which one wins would depend on map iteration order), so
	// the config is unresolvable and boot must fail.
	namespacedOwners := make(map[string]string)
	// Single sorted pass over the results. The workers never log: every
	// connected/failed/skipped line is emitted here in sorted order, keeping
	// the log stream deterministic no matter when each handshake finished.
	for i, name := range names {
		result := results[i]
		if result.err != nil {
			if cfg.Servers[name].Optional {
				logger.Warn(
					"mcp server optional, skipping",
					"server", name,
					"error", result.err,
				)

				continue
			}
			// Survivors connected so far are already on the manager: Close
			// tears their sessions down before startup aborts.
			_ = manager.Close()

			return nil, fmt.Errorf("mcp server %q: %w", name, result.err)
		}
		cl := result.client
		manager.clients = append(manager.clients, cl)
		for _, def := range cl.definitions {
			if owner, ok := namespacedOwners[def.Name]; ok {
				_ = manager.Close()

				return nil, fmt.Errorf(
					"mcp namespaced tool name %q is produced by both server %q and server %q",
					def.Name, owner, name,
				)
			}
			namespacedOwners[def.Name] = name
		}
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

// sortedServerNames returns the configured server names in sorted order.
// cfg.Servers is a Go map, so its iteration order is randomized per run;
// sorting pins the registration order so the tool list exposed to the LLM
// is deterministic across restarts.
func sortedServerNames(servers map[string]config.MCPServerConfig) []string {
	names := make([]string, 0, len(servers))
	for name := range servers {
		names = append(names, name)
	}
	slices.Sort(names)

	return names
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
