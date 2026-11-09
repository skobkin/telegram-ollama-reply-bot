// Package mcp wires the bot to one or more external Model Context Protocol
// (MCP) servers and registers their tools on the existing tooluse.Registry.
package mcp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"telegram-ollama-reply-bot/internal/config"
	"telegram-ollama-reply-bot/internal/tooluse"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// defaultClientTimeout is the per-tool-call timeout used when the operator did
// not set `mcp.servers.<name>.timeout`. Config may accept any positive
// duration; there is deliberately no upper clamp so long-running tools keep
// working.
const defaultClientTimeout = 15 * time.Second

// headerRoundTripper injects the configured headers on outgoing requests to
// the configured endpoint origin. Configured headers use Set semantics so an
// operator-configured value replaces any value the SDK already set (Accept,
// Content-Type, Mcp-Session-Id, MCP-Protocol-Version) instead of producing
// duplicates on the wire. Header values are kept in-memory only and never
// logged.
//
// When origin is non-empty, injection is skipped for requests to any other
// scheme://host — typically redirect hops. Go's client already strips
// well-known sensitive headers (Authorization, Cookie) on cross-host
// redirects, but a transport-level injection would re-add them afterwards,
// sending operator credentials to whatever host the server redirects to.
// Same-host redirects keep the credentials and keep working.
//
// DELETE requests additionally carry a bounded deadline (see
// sessionCleanupTimeout): they terminate the session during Close, run on a
// detached context the SDK never cancels, and would otherwise hang shutdown
// forever against a server that accepts DELETE but never answers.
type headerRoundTripper struct {
	base   http.RoundTripper
	header http.Header
	origin string // normalized "scheme://host" of the configured endpoint; empty = inject on every request
}

// sessionCleanupTimeout bounds session termination twice over: each
// session-termination DELETE round trip at the transport, and the total wait
// for session.Close() in client.close. The transport-level deadline alone is
// not enough: net/http answers a 301/302/303 to a DELETE by re-issuing the
// request as a GET on the redirect target, and those follow-up hops reach
// the transport on a fresh, deadline-free context — a redirect chain ending
// on a hanging server would block Close forever. The SDK also issues the
// DELETE on a detached context with no deadline (it must outlive Connect)
// and this client deliberately sets no http.Client.Timeout so the standing
// SSE stream can live indefinitely, so without these bounds a server
// accepting the DELETE but never answering blocks shutdown and
// optional-server cleanup forever. The bound is a var so tests can shorten
// it.
var sessionCleanupTimeout = 5 * time.Second

func (t *headerRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	injects := t.injectsInto(req)
	bounded := req.Method == http.MethodDelete && sessionCleanupTimeout > 0
	if !injects && !bounded {
		// Fast path: the request reaches the base transport untouched.
		return t.base.RoundTrip(req)
	}

	ctx := req.Context()
	cancel := context.CancelFunc(func() {})
	if bounded {
		ctx, cancel = context.WithTimeout(ctx, sessionCleanupTimeout)
	}

	// Clone the request to avoid mutating the caller's header map.
	cloned := req.Clone(ctx)
	if injects {
		for name, values := range t.header {
			for _, v := range values {
				cloned.Header.Set(name, v)
			}
		}
	}

	resp, err := t.base.RoundTrip(cloned)
	if err != nil {
		cancel()

		return nil, err
	}
	if bounded {
		resp.Body = &cancelOnCloseBody{ReadCloser: resp.Body, cancel: cancel}
	} else {
		// Unbounded requests never wrapped a timeout context; cancel is the
		// no-op fallback here and runs immediately.
		cancel()
	}

	return resp, nil
}

// cancelOnCloseBody releases the bounded cleanup request's context once its
// (small, non-streamed) body is closed. If the caller never closes it, the
// deadline itself reclaims the connection when it fires.
type cancelOnCloseBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *cancelOnCloseBody) Close() error {
	err := b.ReadCloser.Close()
	b.cancel()

	return err
}

// injectsInto reports whether the configured headers may be attached to this
// request. Requests to a different origin than the configured endpoint (an
// off-host redirect hop) never receive the configured credentials.
func (t *headerRoundTripper) injectsInto(req *http.Request) bool {
	if len(t.header) == 0 {
		return false
	}
	if t.origin == "" {
		return true
	}

	return requestOrigin(req) == t.origin
}

// requestOrigin normalizes a request URL to "scheme://host" for comparison
// with headerRoundTripper.origin. The comparison is case-insensitive on both
// parts (schemes and hostnames are case-insensitive per RFC 3986; the port
// keeps its digits).
func requestOrigin(req *http.Request) string {
	if req.URL == nil || req.URL.Host == "" {
		return ""
	}

	return strings.ToLower(req.URL.Scheme) + "://" + strings.ToLower(req.URL.Host)
}

// endpointOrigin normalizes the configured endpoint URL the same way
// requestOrigin does. Returns "" when the URL has no host part (such an
// endpoint fails to connect anyway, so origin scoping must not make things
// worse).
func endpointOrigin(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return ""
	}

	return strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host)
}

// httpClientForServer builds an *http.Client whose transport injects the
// configured headers. It deliberately sets no http.Client.Timeout: the
// Streamable HTTP transport holds a standing SSE GET stream open for the
// lifetime of the session and streams tool-call response bodies, and
// http.Client.Timeout caps the entire request including the body read — a
// wall-clock timeout here would force-kill the session's streams every
// interval. Per-call time bounds are instead applied to each individual
// tool call via callTimeout (see callToolHandler), and session-termination
// DELETEs get a bounded transport-level deadline (see sessionCleanupTimeout).
//
// Redirects to a plain-HTTP target are refused when the configured endpoint
// is HTTPS: the transport would still attach the operator's credentials
// (same host, so origin scoping passes) onto an unencrypted hop. The policy
// also caps redirect chains at maxRedirectHops, restoring Go's default
// limit that a custom CheckRedirect replaces (see checkMCPRedirect).
//
// The base transport is http.DefaultTransport so operators get the same
// proxy / TLS configuration as every other outbound call from the bot.
func httpClientForServer(server config.MCPServerConfig) *http.Client {
	headers := http.Header{}
	for name, value := range server.Headers {
		headers.Set(name, value)
	}

	return &http.Client{
		Transport: &headerRoundTripper{
			base:   http.DefaultTransport,
			header: headers,
			origin: endpointOrigin(server.URL),
		},
		CheckRedirect: checkMCPRedirect,
	}
}

// maxRedirectHops matches net/http's built-in redirect limit. Setting a
// custom CheckRedirect policy replaces the default one, so the limit has to
// be re-implemented here: an uncapped chain would let a redirecting (or
// self-redirecting) endpoint walk the client from hop to hop indefinitely.
const maxRedirectHops = 10

// checkMCPRedirect is an http.Client CheckRedirect policy that keeps Go's
// default redirect-hop cap and refuses any redirect from an HTTPS URL to a
// plain-HTTP URL, regardless of host. Following such a redirect would
// resend credentials over plaintext.
func checkMCPRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= maxRedirectHops {
		return fmt.Errorf("mcp: stopped after %d redirects", maxRedirectHops)
	}
	if len(via) == 0 {
		return nil
	}
	if strings.EqualFold(via[0].URL.Scheme, "https") && strings.EqualFold(req.URL.Scheme, "http") {
		// Log-safe message: scheme and host only, never the full target URL.
		return fmt.Errorf("mcp: refusing redirect from https to http://%s", req.URL.Host)
	}

	return nil
}

// callTimeout returns the per-tool-call deadline derived from the per-server
// override, falling back to the package default when the operator left it
// unset.
func callTimeout(server config.MCPServerConfig) time.Duration {
	if server.Timeout > 0 {
		return server.Timeout
	}

	return defaultClientTimeout
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
		// The endpoint URL may carry credentials as query parameters, and the
		// SDK error quotes it verbatim; redact before the error reaches logs
		// or the startup failure message.
		return fmt.Errorf("connect to %s: %s", redactURLCredentials(c.config.URL), redactURLCredentials(err.Error()))
	}
	c.session = session

	tools, err := c.listTools(ctx)
	if err != nil {
		_ = session.Close()

		return fmt.Errorf("list tools from %s: %s", redactURLCredentials(c.config.URL), redactURLCredentials(err.Error()))
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
		// captures the live session, the un-prefixed tool name, and the
		// per-server call timeout so the tooluse runtime can dispatch without
		// knowing about MCP.
		definition.Handler = callToolHandler(
			session,
			tool.Name,
			definition.Name,
			callTimeout(c.config),
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
// times; subsequent calls are no-ops. Close errors can quote the request
// URL (see redactURLCredentials) and are redacted before they reach logs.
//
// The wait for the SDK's Close is bounded by sessionCleanupTimeout on top of
// the per-DELETE transport deadline: net/http re-issues 301/302/303 answers
// to the termination DELETE as GETs on the redirect target, and those
// follow-up hops carry no deadline of their own. On timeout the cleanup is
// abandoned in the background (the SDK's closeOnce keeps a late finish from
// double-closing) and a URL-free error is returned so shutdown can proceed.
func (c *client) close() error {
	if c == nil || c.session == nil {
		return nil
	}
	// Capture the session before nil-ing the field: the goroutine below must
	// never touch c.session after close has cleared it.
	session := c.session
	c.session = nil

	done := make(chan error, 1)
	go func() {
		done <- session.Close()
	}()

	timer := time.NewTimer(sessionCleanupTimeout)
	defer timer.Stop()

	select {
	case err := <-done:
		if err != nil {
			err = errors.New(redactURLCredentials(err.Error()))
		}

		return err
	case <-timer.C:
		serverLogger(c.logger, c.name).Warn(
			"mcp session cleanup timed out; abandoned and may keep running in the background",
			"timeout", sessionCleanupTimeout,
		)

		return fmt.Errorf("mcp session cleanup timed out after %s", sessionCleanupTimeout)
	}
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
