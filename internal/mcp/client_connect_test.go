package mcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"telegram-ollama-reply-bot/internal/config"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// newEchoMCPServer returns a minimal in-process MCP server advertising a
// single "echo" tool, ready to be served through mcp.NewStreamableHTTPHandler.
func newEchoMCPServer() *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{Name: "test-server", Version: "1.0.0"}, nil)
	srv.AddTool(
		&mcp.Tool{Name: "echo", InputSchema: map[string]any{"type": "object"}},
		func(_ context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil
		},
	)

	return srv
}

// TestStreamableTransportForCarriesReconnectBudget pins the SSE reconnect
// budget: the transport must keep the SDK default of 5 reconnect attempts
// (backing off 1s ×1.5 with full jitter, capped at 30s) and must carry the
// httpClientForServer-built HTTP client, so a single transient drop of the
// standing server→client notification stream is retried instead of silently
// killing that stream for the rest of the session.
func TestStreamableTransportForCarriesReconnectBudget(t *testing.T) {
	cfg := config.MCPServerConfig{
		URL:     "http://mcp.example.invalid/mcp",
		Headers: map[string]string{"Authorization": "Bearer opaque"},
	}

	tr := streamableTransportFor(cfg)
	if tr.MaxRetries != 5 {
		t.Fatalf("MaxRetries = %d, want 5 (the SDK default reconnect budget)", tr.MaxRetries)
	}
	if tr.Endpoint != cfg.URL {
		t.Fatalf("Endpoint = %q, want %q", tr.Endpoint, cfg.URL)
	}
	if tr.HTTPClient == nil {
		t.Fatal("expected the transport to carry the httpClientForServer client")
	}
	if _, ok := tr.HTTPClient.Transport.(*headerRoundTripper); !ok {
		t.Fatalf("expected httpClientForServer transport, got %T", tr.HTTPClient.Transport)
	}
	if got := tr.HTTPClient.Transport.(*headerRoundTripper).header.Get("Authorization"); got != "Bearer opaque" {
		t.Fatalf("expected configured headers on the carried client, got %q", got)
	}
}

// TestConnectBoundsSSEHeaderWait pins the handshake bound: a server that
// answers the initialize POST but stalls the standing SSE GET before writing
// response headers must not block connect forever. The SDK issues that GET on
// a detached connection context (go-sdk v1.2.0, streamable.go), so the
// caller's bootstrap deadline never reaches it — only the transport's
// ResponseHeaderWaitTimeout can end the wait. Before the fix, Connect blocked
// here until the connection died on its own (>90s in the original
// reproduction).
func TestConnectBoundsSSEHeaderWait(t *testing.T) {
	original := responseHeaderWaitTimeout
	responseHeaderWaitTimeout = 100 * time.Millisecond
	t.Cleanup(func() { responseHeaderWaitTimeout = original })

	// Trim the reconnect budget too: with the full SDK-default budget of 5,
	// Connect legitimately spends ~14-28s walking the jittered backoff ladder
	// between GET attempts. This test pins the per-attempt header wait, so one
	// attempt + one retry (the pre-fix wiring) keeps the watchdog tight.
	originalBudget := sseReconnectBudget
	sseReconnectBudget = 1
	t.Cleanup(func() { sseReconnectBudget = originalBudget })

	inner := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return newEchoMCPServer() }, nil)

	release := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			// Stall the standing SSE GET before writing any response
			// header: the header wait has to be what unblocks Connect.
			select {
			case <-release:
			case <-r.Context().Done():
			}

			return
		}
		inner.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)
	// Registered after ts.Close so cleanup LIFO order releases the stalled
	// GET first: an abandoned GET never cancels its request context, so
	// context-only release would deadlock the server shutdown.
	t.Cleanup(func() { close(release) })

	raw := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "1.0.0"}, nil)
	c := &client{
		name:   "sse-header-probe",
		config: config.MCPServerConfig{URL: ts.URL},
		raw:    raw,
		logger: discardLogger(),
	}
	// No deadline on the bootstrap context on purpose: it never reaches the
	// detached SSE GET, and this test must not depend on it.
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	// Watchdog headroom: after the first GET fails on the header timeout, the
	// SDK reconnects once with 1-2s of backoff before giving up, so the total
	// connect time is ~1.5-2.5s.
	done := make(chan error, 1)
	go func() { done <- c.connect(ctx) }()

	var err error
	select {
	case err = <-done:
	case <-time.After(4 * time.Second):
		t.Fatal("connect blocked >4s waiting for SSE response headers; the header wait is unbounded")
	}
	if err == nil {
		t.Fatal("expected connect to fail once the SSE stream request timed out")
	}
}

// TestConnectFailureCleanupBoundedOnRedirectedDelete pins the discovery-
// failure cleanup bound: connect() closes the just-opened session itself when
// tool discovery fails (list tools, filter tools, translate), and those paths
// must not block startup forever. Here discovery fails on a
// RestrictedTools typo, and the session-termination DELETE is answered with a
// 302 to a GET that never responds — the redirected hop carries no deadline,
// so only the total close wait can end the cleanup. Before the fix,
// connect() hung indefinitely inside session.Close().
func TestConnectFailureCleanupBoundedOnRedirectedDelete(t *testing.T) {
	original := sessionCleanupTimeout
	sessionCleanupTimeout = 100 * time.Millisecond
	t.Cleanup(func() { sessionCleanupTimeout = original })

	inner := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return newEchoMCPServer() }, nil)

	releaseSink := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodDelete:
			http.Redirect(w, r, "/sink", http.StatusFound)
		case r.URL.Path == "/sink":
			// The redirected GET carries no deadline, so the abandoned
			// client never closes its connection: waiting on
			// r.Context().Done() alone would deadlock ts.Close, which only
			// force-closes idle connections. The test releases the sink just
			// before shutting the server down.
			select {
			case <-releaseSink:
			case <-r.Context().Done():
			}
		default:
			inner.ServeHTTP(w, r)
		}
	}))
	t.Cleanup(ts.Close)
	// Registered after ts.Close so cleanup LIFO order releases the sink
	// first: that lets the abandoned session.Close() finish, which in turn
	// tears down the SDK's standing SSE stream before the server waits on
	// its connections.
	t.Cleanup(func() { close(releaseSink) })

	raw := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "1.0.0"}, nil)
	// "nope" is not advertised by the server, so filterTools fails by design.
	c := &client{
		name:   "connect-failure-probe",
		config: config.MCPServerConfig{URL: ts.URL, RestrictedTools: []string{"nope"}},
		raw:    raw,
		logger: discardLogger(),
	}
	ctx, cancel := context.WithTimeout(context.Background(), testConnectTimeout)
	t.Cleanup(cancel)

	done := make(chan error, 1)
	go func() { done <- c.connect(ctx) }()

	var err error
	select {
	case err = <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("connect blocked >2s on discovery-failure cleanup; the unbounded session.Close() hang is back")
	}
	if err == nil {
		t.Fatal("expected connect to report the tool filter failure")
	}
	// The failure must be the filter error, not the (discarded) cleanup error.
	if !strings.Contains(err.Error(), `restricted tool "nope"`) {
		t.Fatalf("expected the filter error for %q, got %q", "nope", err.Error())
	}
}
