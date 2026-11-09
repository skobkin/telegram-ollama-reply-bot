package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"telegram-ollama-reply-bot/internal/config"
	"telegram-ollama-reply-bot/internal/tooluse"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	// testCallTimeout is the tiny per-call timeout used to exercise the
	// deadline path without slowing the suite down.
	testCallTimeout = 50 * time.Millisecond
	// slowToolDelay must comfortably exceed testCallTimeout so the deadline
	// reliably fires before the tool returns.
	slowToolDelay = 250 * time.Millisecond
	// testConnectTimeout bounds the whole test session lifetime.
	testConnectTimeout = 5 * time.Second
)

// startCallTestServer spins up an in-process MCP server behind an httptest
// server exposing the Streamable HTTP transport. It registers:
//
//   - "echo": returns "echo:<text>" immediately
//   - "slow": sleeps slowToolDelay before returning
//
// The returned httptest server is closed automatically when the test ends.
func startCallTestServer(t *testing.T) *httptest.Server {
	t.Helper()

	srv := mcp.NewServer(&mcp.Implementation{Name: "test-server", Version: "1.0.0"}, nil)

	srv.AddTool(
		&mcp.Tool{Name: "echo", InputSchema: map[string]any{"type": "object"}},
		func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			var args struct {
				Text string `json:"text"`
			}
			if len(req.Params.Arguments) > 0 {
				if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
					return nil, err
				}
			}

			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: "echo:" + args.Text}},
			}, nil
		},
	)

	srv.AddTool(
		&mcp.Tool{Name: "slow", InputSchema: map[string]any{"type": "object"}},
		func(_ context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			time.Sleep(slowToolDelay)

			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: "slow done"}},
			}, nil
		},
	)

	ts := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil))
	t.Cleanup(ts.Close)

	return ts
}

// connectCallTestClient opens a client session against the test server using
// the production httpClientForServer transport wiring. The session is closed
// automatically when the test finishes.
func connectCallTestClient(t *testing.T, ts *httptest.Server, serverCfg config.MCPServerConfig) *mcp.ClientSession {
	t.Helper()

	raw := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "1.0.0"}, nil)
	transport := &mcp.StreamableClientTransport{
		Endpoint:   ts.URL,
		HTTPClient: httpClientForServer(serverCfg),
		MaxRetries: 1,
	}

	ctx, cancel := context.WithTimeout(context.Background(), testConnectTimeout)
	t.Cleanup(cancel)

	session, err := raw.Connect(ctx, transport, nil)
	if err != nil {
		t.Fatalf("connect test client: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })

	return session
}

// TestCallToolHandlerTimesOutSlowTool pins the per-call timeout contract: a
// tool that takes longer than the configured timeout fails the call with the
// structured `mcp_unreachable` error, and — crucially — the session survives
// so subsequent fast calls still work. Before the per-call timeout was
// introduced this bound was enforced by http.Client.Timeout, which
// additionally force-killed the standing SSE stream on every expiry.
func TestCallToolHandlerTimesOutSlowTool(t *testing.T) {
	ts := startCallTestServer(t)
	session := connectCallTestClient(t, ts, config.MCPServerConfig{Timeout: testCallTimeout})

	timeout := callTimeout(config.MCPServerConfig{Timeout: testCallTimeout})
	handler := callToolHandler(session, "slow", "mcp_test__slow", timeout, nil)

	if _, err := handler(context.Background(), tooluse.CallContext{}, nil); err == nil {
		t.Fatalf("expected timeout error for slow tool")
	} else if !strings.Contains(err.Error(), "mcp_unreachable") {
		t.Fatalf("expected mcp_unreachable error, got: %v", err)
	}

	// The session must still be usable: a fast call succeeds after the
	// timed-out one.
	fast := callToolHandler(session, "echo", "mcp_test__echo", timeout, nil)
	result, err := fast(context.Background(), tooluse.CallContext{}, []byte(`{"text":"hi"}`))
	if err != nil {
		t.Fatalf("fast call after timeout failed: %v", err)
	}
	data, ok := result.Data.(map[string]any)
	if !ok {
		t.Fatalf("expected map[string]any data, got %T", result.Data)
	}
	if data["text"] != "echo:hi" {
		t.Fatalf("unexpected echo result: %v", data["text"])
	}
}

// TestCallToolHandlerSucceedsForFastTool verifies that a tool completing well
// inside the per-call timeout returns a normal "ok" result.
func TestCallToolHandlerSucceedsForFastTool(t *testing.T) {
	ts := startCallTestServer(t)
	session := connectCallTestClient(t, ts, config.MCPServerConfig{Timeout: testCallTimeout})

	handler := callToolHandler(session, "echo", "mcp_test__echo", callTimeout(config.MCPServerConfig{Timeout: testCallTimeout}), nil)

	result, err := handler(context.Background(), tooluse.CallContext{}, []byte(`{"text":"ping"}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status != "ok" {
		t.Fatalf("expected status=ok, got %q", result.Status)
	}
	data := result.Data.(map[string]any)
	if data["text"] != "echo:ping" {
		t.Fatalf("unexpected echo result: %v", data["text"])
	}
}

// TestCallTimeoutDefaultsWhenUnset checks the fallback to the package default
// when the operator left the per-server timeout unset.
func TestCallTimeoutDefaultsWhenUnset(t *testing.T) {
	if got := callTimeout(config.MCPServerConfig{}); got != defaultClientTimeout {
		t.Fatalf("expected default timeout %v, got %v", defaultClientTimeout, got)
	}
	if got := callTimeout(config.MCPServerConfig{Timeout: 3 * time.Second}); got != 3*time.Second {
		t.Fatalf("expected configured timeout to win, got %v", got)
	}
}
