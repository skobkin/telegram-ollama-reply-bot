package mcp

import (
	"net/http"
	"reflect"
	"testing"

	"telegram-ollama-reply-bot/internal/config"
)

// capturingRoundTripper records the last request it received instead of
// performing any network I/O.
type capturingRoundTripper struct {
	requests []*http.Request
}

func (c *capturingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	c.requests = append(c.requests, req)

	return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Header: http.Header{}}, nil
}

// TestHeaderRoundTripperReplacesExistingHeaders pins Set semantics: an
// operator-configured header must replace any value the SDK already set on
// the request (Accept, Content-Type, Mcp-Session-Id, MCP-Protocol-Version),
// never append a duplicate.
func TestHeaderRoundTripperReplacesExistingHeaders(t *testing.T) {
	base := &capturingRoundTripper{}
	rt := &headerRoundTripper{
		base:   base,
		header: http.Header{"Accept": []string{"application/vnd.operator+json"}},
	}

	req, err := http.NewRequest(http.MethodPost, "https://mcp.example.invalid", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	// Headers the SDK sets on its own requests (streamable.go Write /
	// connectSSE / setMCPHeaders).
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Mcp-Session-Id", "session-1")

	if _, err := rt.RoundTrip(req); err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if len(base.requests) != 1 {
		t.Fatalf("expected 1 captured request, got %d", len(base.requests))
	}

	sent := base.requests[0]
	accept := sent.Header.Values("Accept")
	if len(accept) != 1 {
		t.Fatalf("expected exactly one Accept value, got %v", accept)
	}
	if accept[0] != "application/vnd.operator+json" {
		t.Fatalf("expected operator Accept to win, got %q", accept[0])
	}
	if got := sent.Header.Get("Content-Type"); got != "application/json" {
		t.Fatalf("expected untouched Content-Type to survive, got %q", got)
	}
	if got := sent.Header.Get("Mcp-Session-Id"); got != "session-1" {
		t.Fatalf("expected untouched Mcp-Session-Id to survive, got %q", got)
	}
}

// TestHeaderRoundTripperSetsAllConfiguredHeaders verifies every configured
// header lands on the request, including names the SDK did not set.
func TestHeaderRoundTripperSetsAllConfiguredHeaders(t *testing.T) {
	base := &capturingRoundTripper{}
	rt := &headerRoundTripper{
		base: base,
		header: http.Header{
			"Accept":            []string{"application/vnd.operator+json"},
			"X-Custom-Trace":    []string{"trace-42"},
			"Mcp-Session-Id":    []string{"operator-session"},
			"Mcp-Protocol-Vers": []string{"2025-06-18"},
		},
	}

	req, err := http.NewRequest(http.MethodGet, "https://mcp.example.invalid", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Mcp-Session-Id", "sdk-session")
	req.Header.Set("Mcp-Protocol-Vers", "2025-03-26")

	if _, err := rt.RoundTrip(req); err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}

	sent := base.requests[0]
	for name, want := range map[string]string{
		"Accept":            "application/vnd.operator+json",
		"X-Custom-Trace":    "trace-42",
		"Mcp-Session-Id":    "operator-session",
		"Mcp-Protocol-Vers": "2025-06-18",
	} {
		got := sent.Header.Values(name)
		if len(got) != 1 || got[0] != want {
			t.Fatalf("header %s: want exactly [%s], got %v", name, want, got)
		}
	}
}

// TestHeaderRoundTripperPassthroughWhenEmpty verifies the fast path: with no
// configured headers the request reaches the base transport untouched.
func TestHeaderRoundTripperPassthroughWhenEmpty(t *testing.T) {
	base := &capturingRoundTripper{}
	rt := &headerRoundTripper{base: base}

	req, err := http.NewRequest(http.MethodGet, "https://mcp.example.invalid", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Accept", "text/event-stream")

	if _, err := rt.RoundTrip(req); err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if len(base.requests) != 1 || base.requests[0] != req {
		t.Fatalf("expected the original request to reach the base transport")
	}
	if got := base.requests[0].Header.Get("Accept"); got != "text/event-stream" {
		t.Fatalf("expected untouched Accept, got %q", got)
	}
}

// TestHttpClientForServerKeepsOriginalRequestHeaders verifies the client
// wiring does not mutate the caller's config map and normalizes the header
// map once.
func TestHttpClientForServerKeepsOriginalRequestHeaders(t *testing.T) {
	cfg := config.MCPServerConfig{
		Headers: map[string]string{"Authorization": "Bearer opaque", "Accept": "application/vnd.operator+json"},
	}
	client := httpClientForServer(cfg)
	rt, ok := client.Transport.(*headerRoundTripper)
	if !ok {
		t.Fatalf("expected headerRoundTripper transport, got %T", client.Transport)
	}
	if client.Timeout != 0 {
		t.Fatalf("expected no client-level timeout, got %v", client.Timeout)
	}
	if !reflect.DeepEqual(rt.header.Get("Authorization"), "Bearer opaque") {
		t.Fatalf("unexpected Authorization header: %q", rt.header.Get("Authorization"))
	}
}
