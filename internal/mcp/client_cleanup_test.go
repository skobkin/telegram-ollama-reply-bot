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

// blockingRoundTripper records that it was entered, then blocks until its
// request context is cancelled — simulating a server that accepted the
// connection but never answers.
type blockingRoundTripper struct {
	entered chan struct{}
}

func (b *blockingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	select {
	case <-b.entered:
	default:
		close(b.entered)
	}
	<-req.Context().Done()

	return nil, req.Context().Err()
}

// deadlineInspectingRoundTripper records whether the request context carried
// a deadline.
type deadlineInspectingRoundTripper struct {
	hadDeadline bool
}

func (d *deadlineInspectingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	_, ok := req.Context().Deadline()
	d.hadDeadline = ok

	return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Header: http.Header{}}, nil
}

// TestHeaderRoundTripperBoundsDeleteRequests pins the cleanup bound: a DELETE
// that the base transport never answers must abort when the cleanup deadline
// fires, not hang forever.
func TestHeaderRoundTripperBoundsDeleteRequests(t *testing.T) {
	original := sessionCleanupTimeout
	sessionCleanupTimeout = 50 * time.Millisecond
	t.Cleanup(func() { sessionCleanupTimeout = original })

	base := &blockingRoundTripper{entered: make(chan struct{})}
	rt := &headerRoundTripper{base: base}

	req, err := http.NewRequest(http.MethodDelete, "https://mcp.example.invalid", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		resp, err := rt.RoundTrip(req)
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected the bounded DELETE to fail with a context error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("bounded DELETE did not return within 2s; cleanup still hangs")
	}
}

// TestHeaderRoundTripperLeavesNonDeleteUnbounded verifies the bound applies
// only to DELETE: GET and POST requests keep deadline-free contexts so the
// standing SSE stream and per-call-timed tool calls are unaffected.
func TestHeaderRoundTripperLeavesNonDeleteUnbounded(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		base := &deadlineInspectingRoundTripper{}
		rt := &headerRoundTripper{base: base}

		req, err := http.NewRequest(method, "https://mcp.example.invalid", nil)
		if err != nil {
			t.Fatalf("build request: %v", err)
		}

		resp, err := rt.RoundTrip(req)
		if err != nil {
			t.Fatalf("%s RoundTrip: %v", method, err)
		}
		_ = resp.Body.Close()
		if base.hadDeadline {
			t.Fatalf("%s request unexpectedly carried a deadline", method)
		}
	}
}

// TestClientCloseBoundsHangingDeleteCleanup pins the shutdown behavior end to
// end: against a server that completes the MCP handshake but never answers
// the session-termination DELETE, client.close() must still return promptly
// once the cleanup deadline fires — before the fix, session.Close() blocked
// indefinitely there.
func TestClientCloseBoundsHangingDeleteCleanup(t *testing.T) {
	original := sessionCleanupTimeout
	sessionCleanupTimeout = 100 * time.Millisecond
	t.Cleanup(func() { sessionCleanupTimeout = original })

	srv := mcp.NewServer(&mcp.Implementation{Name: "test-server", Version: "1.0.0"}, nil)
	srv.AddTool(
		&mcp.Tool{Name: "echo", InputSchema: map[string]any{"type": "object"}},
		func(_ context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil
		},
	)
	inner := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			// Stall the cleanup request until the client gives up: the
			// connection close cancels this request context, so the test
			// server can still shut down cleanly.
			<-r.Context().Done()

			return
		}
		inner.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)

	raw := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "1.0.0"}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), testConnectTimeout)
	t.Cleanup(cancel)

	c := &client{
		name:   "cleanup-probe",
		config: config.MCPServerConfig{URL: ts.URL},
		raw:    raw,
		logger: discardLogger(),
	}
	if err := c.connect(ctx); err != nil {
		t.Fatalf("connect: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- c.close() }()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("close() blocked >3s on a hanging DELETE; shutdown hang is back")
	}
	if c.session != nil {
		t.Fatal("expected session to be cleared after close")
	}
}

// TestClientCloseBoundsRedirectedCleanup pins the gap the transport-level
// DELETE deadline cannot cover: a server answering the session-termination
// DELETE with a 302 makes net/http re-issue it as a GET on the redirect
// target, and that follow-up hop carries no deadline. The total close wait
// must fire anyway, returning a URL-free error instead of blocking shutdown
// forever on a hanging redirect target.
func TestClientCloseBoundsRedirectedCleanup(t *testing.T) {
	original := sessionCleanupTimeout
	sessionCleanupTimeout = 100 * time.Millisecond
	t.Cleanup(func() { sessionCleanupTimeout = original })

	srv := mcp.NewServer(&mcp.Implementation{Name: "test-server", Version: "1.0.0"}, nil)
	srv.AddTool(
		&mcp.Tool{Name: "echo", InputSchema: map[string]any{"type": "object"}},
		func(_ context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil
		},
	)
	inner := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil)

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
	ctx, cancel := context.WithTimeout(context.Background(), testConnectTimeout)
	t.Cleanup(cancel)

	c := &client{
		name:   "redirect-cleanup-probe",
		config: config.MCPServerConfig{URL: ts.URL},
		raw:    raw,
		logger: discardLogger(),
	}
	if err := c.connect(ctx); err != nil {
		t.Fatalf("connect: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- c.close() }()

	var err error
	select {
	case err = <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("close() blocked >2s on a redirected cleanup; the deadline does not survive the 302")
	}
	if err == nil {
		t.Fatal("expected close to report the abandoned cleanup")
	}
	if !strings.Contains(err.Error(), "cleanup timed out") {
		t.Fatalf("expected the error to mention the cleanup timeout, got %q", err.Error())
	}
	if strings.Contains(err.Error(), ts.URL) {
		t.Fatalf("timeout error must not echo any URL: %q", err.Error())
	}
	if c.session != nil {
		t.Fatal("expected session to be cleared after close")
	}
}
