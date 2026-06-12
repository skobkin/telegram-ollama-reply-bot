// Package mcp wires the bot to one or more external Model Context Protocol
// (MCP) servers and registers their tools on the existing tooluse.Registry.
package mcp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"telegram-ollama-reply-bot/internal/config"
	"telegram-ollama-reply-bot/internal/tooluse"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// defaultClientTimeout is the per-request timeout used when the operator did
// not set `mcp.servers.<name>.timeout`. It is intentionally shorter than the
// per-server maximum (60s) so a single misconfigured server cannot stall the
// chat loop.
const defaultClientTimeout = 15 * time.Second

// headerRoundTripper injects the configured headers on every outgoing
// request. Header values are kept in-memory only and never logged.
type headerRoundTripper struct {
	base   http.RoundTripper
	header http.Header
}

func (t *headerRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if len(t.header) == 0 {
		return t.base.RoundTrip(req)
	}
	// Clone the request to avoid mutating the caller's header map.
	cloned := req.Clone(req.Context())
	cloned.Header = req.Header.Clone()
	for name, values := range t.header {
		for _, v := range values {
			cloned.Header.Add(name, v)
		}
	}

	return t.base.RoundTrip(cloned)
}

// httpClientForServer builds an *http.Client whose timeout matches the
// per-server override (or the package default) and whose transport injects
// the configured headers. The base transport is http.DefaultTransport so
// operators get the same proxy / TLS configuration as every other
// outbound call from the bot.
func httpClientForServer(server config.MCPServerConfig) *http.Client {
	timeout := server.Timeout
	if timeout <= 0 {
		timeout = defaultClientTimeout
	}

	headers := http.Header{}
	for name, value := range server.Headers {
		headers.Set(name, value)
	}

	return &http.Client{
		Timeout: timeout,
		Transport: &headerRoundTripper{
			base:   http.DefaultTransport,
			header: headers,
		},
	}
}

// client wraps the MCP SDK's *Client and the open ClientSession for a single
// server. The struct is intentionally thin: it owns the lifecycle (Connect /
// Close) and exposes the toolset. The Manager coordinates many of these.
type client struct {
	name    string
	config  config.MCPServerConfig
	raw     *mcp.Client
	session *mcp.ClientSession
	logger  *slog.Logger

	// definitions is the post-filter, translated list of tooluse.Definitions
	// this server contributes. The slice is computed once at startup and is
	// safe to read concurrently; tool dispatch goes through the captured
	// `Handler` closures inside each Definition.
	definitions []tooluse.Definition
}

// connect opens a Streamable HTTP connection to the configured MCP server,
// lists its tools, applies the per-server allow-then-restrict filter, and
// translates each survivor into a tooluse.Definition with a Handler closure
// routed back to the new session.
//
// The returned error is the one a human would read at startup; the Manager
// turns it into a warning + zero-tool server when the server is `Optional`.
func (c *client) connect(ctx context.Context) error {
	if c.raw == nil {
		return errors.New("mcp client is not initialised")
	}

	transport := &mcp.StreamableClientTransport{
		Endpoint:   c.config.URL,
		HTTPClient: httpClientForServer(c.config),
		MaxRetries: 1,
	}

	session, err := c.raw.Connect(ctx, transport, nil)
	if err != nil {
		return fmt.Errorf("connect to %s: %w", c.config.URL, err)
	}
	c.session = session

	tools, err := c.listTools(ctx)
	if err != nil {
		_ = session.Close()

		return fmt.Errorf("list tools from %s: %w", c.config.URL, err)
	}

	names := make([]string, 0, len(tools))
	byName := make(map[string]*mcp.Tool, len(tools))
	for _, tool := range tools {
		names = append(names, tool.Name)
		byName[tool.Name] = tool
	}

	allowed, err := filterTools(names, c.config.AllowedTools, c.config.RestrictedTools)
	if err != nil {
		_ = session.Close()

		return fmt.Errorf("filter tools for %s: %w", c.name, err)
	}

	definitions := make([]tooluse.Definition, 0, len(allowed))
	for _, name := range allowed {
		tool, ok := byName[name]
		if !ok || tool == nil {
			continue
		}
		definition, err := translateDefinition(c.name, c.config, tool)
		if err != nil {
			_ = session.Close()

			return fmt.Errorf("translate %s/%s: %w", c.name, name, err)
		}
		// Wire the call closure now that the session is open. The handler
		// captures the live session and the un-prefixed tool name so the
		// tooluse runtime can dispatch without knowing about MCP.
		definition.Handler = callToolHandler(
			session,
			tool.Name,
			definition.Name,
			serverLogger(c.logger, c.name),
		)
		definitions = append(definitions, definition)
	}
	c.definitions = definitions

	return nil
}

// listTools walks the paginated Tools iterator and collects every tool the
// server advertises. The iterator is provided by the SDK; this loop simply
// drains it eagerly so the Manager can synchronously know what the server
// has at startup.
func (c *client) listTools(ctx context.Context) ([]*mcp.Tool, error) {
	tools := make([]*mcp.Tool, 0)
	for tool, err := range c.session.Tools(ctx, nil) {
		if err != nil {
			return nil, fmt.Errorf("paginate tools: %w", err)
		}
		if tool == nil {
			continue
		}
		tools = append(tools, tool)
	}

	return tools, nil
}

// close releases the underlying session, if any. Safe to call multiple
// times; subsequent calls are no-ops.
func (c *client) close() error {
	if c == nil || c.session == nil {
		return nil
	}
	err := c.session.Close()
	c.session = nil

	return err
}

// serverLogger returns a *slog.Logger that already has the per-server
// `server` field attached. Falls back to slog.Default when the base logger
// is nil so callers can always treat the result as safe to invoke.
func serverLogger(base *slog.Logger, name string) *slog.Logger {
	if base == nil {
		base = slog.Default()
	}

	return base.With("server", name)
}
