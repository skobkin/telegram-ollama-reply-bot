package mcp

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
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

	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	defer resp.Body.Close()
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

	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	defer resp.Body.Close()

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

	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	defer resp.Body.Close()
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

// TestHeaderRoundTripperDoesNotInjectAcrossRedirect pins the credential
// scoping: a redirect to another host (a different httptest URL, i.e. a
// different host:port origin) must NOT receive the configured headers —
// neither the ones Go would strip anyway (Authorization) nor arbitrary
// operator headers (X-Api-Key) that Go copies verbatim.
func TestHeaderRoundTripperDoesNotInjectAcrossRedirect(t *testing.T) {
	var targetGot http.Header
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetGot = r.Header.Clone()
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(target.Close)

	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/elsewhere", http.StatusFound)
	}))
	t.Cleanup(source.Close)

	client := &http.Client{
		Transport: &headerRoundTripper{
			base:   http.DefaultTransport,
			header: http.Header{"Authorization": []string{"Bearer secret"}, "X-Api-Key": []string{"hunter2"}},
			origin: endpointOrigin(source.URL),
		},
		CheckRedirect: rejectHTTPSDowngrade,
	}

	resp, err := client.Get(source.URL + "/start")
	if err != nil {
		t.Fatalf("follow redirect: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.ReadAll(resp.Body)

	if got := targetGot.Get("Authorization"); got != "" {
		t.Fatalf("credential leaked to redirect target: Authorization=%q", got)
	}
	if got := targetGot.Get("X-Api-Key"); got != "" {
		t.Fatalf("credential leaked to redirect target: X-Api-Key=%q", got)
	}
}

// TestHeaderRoundTripperInjectsOnSameOriginRedirect verifies same-origin
// redirect hops keep the configured credentials, so legitimately redirecting
// servers keep working.
func TestHeaderRoundTripperInjectsOnSameOriginRedirect(t *testing.T) {
	var targetGot http.Header
	inner := http.NewServeMux()
	inner.HandleFunc("/final", func(w http.ResponseWriter, r *http.Request) {
		targetGot = r.Header.Clone()
		_, _ = w.Write([]byte("ok"))
	})
	inner.HandleFunc("/start", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/final", http.StatusFound)
	})
	source := httptest.NewServer(inner)
	t.Cleanup(source.Close)

	client := &http.Client{
		Transport: &headerRoundTripper{
			base:   http.DefaultTransport,
			header: http.Header{"Authorization": []string{"Bearer secret"}},
			origin: endpointOrigin(source.URL),
		},
		CheckRedirect: rejectHTTPSDowngrade,
	}

	resp, err := client.Get(source.URL + "/start")
	if err != nil {
		t.Fatalf("follow redirect: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.ReadAll(resp.Body)

	if got := targetGot.Get("Authorization"); got != "Bearer secret" {
		t.Fatalf("expected Authorization on same-origin redirect, got %q", got)
	}
}

// TestEndpointOrigin pins the origin normalization used for header scoping.
func TestEndpointOrigin(t *testing.T) {
	cases := []struct {
		raw  string
		want string
	}{
		{"https://MCP.Example.COM:8443/mcp?api_key=x", "https://mcp.example.com:8443"},
		{"HTTP://localhost:9000/mcp", "http://localhost:9000"},
		{"not a url", ""},
		{"https:///no-host", ""},
	}
	for _, tc := range cases {
		if got := endpointOrigin(tc.raw); got != tc.want {
			t.Errorf("endpointOrigin(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

// TestRejectHTTPSDowngrade pins the redirect policy: an HTTPS endpoint must
// never be followed to a plain-HTTP target, on any host. Same- or upgraded-
// scheme redirects stay allowed.
func TestRejectHTTPSDowngrade(t *testing.T) {
	via := []*http.Request{{URL: mustParseURL(t, "https://mcp.example.invalid/mcp")}}

	downgrade, err := http.NewRequest(http.MethodGet, "http://mcp.example.invalid/mcp", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	err = rejectHTTPSDowngrade(downgrade, via)
	if err == nil {
		t.Fatal("expected https to http redirect to be refused")
	}
	if strings.Contains(err.Error(), "api_key") {
		t.Fatalf("redirect refusal must not echo the target URL: %v", err)
	}

	crossHostHTTPS, err := http.NewRequest(http.MethodGet, "https://other.example.invalid/mcp", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if err := rejectHTTPSDowngrade(crossHostHTTPS, via); err != nil {
		t.Fatalf("expected https to https redirect to be allowed, got %v", err)
	}
}

func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()

	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}

	return u
}
