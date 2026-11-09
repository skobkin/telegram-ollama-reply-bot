package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"telegram-ollama-reply-bot/internal/config"
	"telegram-ollama-reply-bot/internal/tooluse"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestCallToolHandlerRedactsTransportError pins the end-to-end behavior the
// review flagged: a mid-call transport failure produces an error whose text —
// the same text that reaches WARN logs and the LLM-visible tool response —
// carries neither the endpoint's query credentials nor the query at all.
func TestCallToolHandlerRedactsTransportError(t *testing.T) {
	const querySecret = "hunter2-secret-query"

	srv := mcp.NewServer(&mcp.Implementation{Name: "test-server", Version: "1.0.0"}, nil)
	srv.AddTool(
		&mcp.Tool{Name: "echo", InputSchema: map[string]any{"type": "object"}},
		func(_ context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil
		},
	)
	inner := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil)

	var killNextCall atomic.Bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && killNextCall.Load() {
			body, _ := io.ReadAll(r.Body)
			if strings.Contains(string(body), `"tools/call"`) {
				if hj, ok := w.(http.Hijacker); ok {
					conn, _, err := hj.Hijack()
					if err == nil {
						_ = conn.Close() // abrupt reset mid-call

						return
					}
				}
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
		}
		inner.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)

	endpoint := ts.URL + "?api_key=" + querySecret
	raw := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "1.0.0"}, nil)
	transport := &mcp.StreamableClientTransport{
		Endpoint:   endpoint,
		HTTPClient: httpClientForServer(config.MCPServerConfig{URL: endpoint}),
		MaxRetries: 1,
	}
	ctx, cancel := context.WithTimeout(context.Background(), testConnectTimeout)
	t.Cleanup(cancel)
	session, err := raw.Connect(ctx, transport, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })

	handler := callToolHandler(session, "echo", "mcp_test__echo", time.Second, discardLogger())
	killNextCall.Store(true)
	_, callErr := handler(context.Background(), tooluse.CallContext{RequestID: "redact-test"}, json.RawMessage(`{}`))
	if callErr == nil {
		t.Fatal("expected transport failure to surface as an error")
	}

	msg := callErr.Error()
	if !strings.Contains(msg, "mcp_unreachable") {
		t.Fatalf("expected mcp_unreachable error, got: %s", msg)
	}
	if strings.Contains(msg, querySecret) {
		t.Fatalf("credential leaked in call error: %s", msg)
	}
	if strings.Contains(msg, "?api_key") || strings.Contains(msg, "?") {
		t.Fatalf("query string survived redaction: %s", msg)
	}
}
