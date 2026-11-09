package mcp

import (
	"context"
	"net/http"
	"net/http/httptest"
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
