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
		CheckRedirect: checkMCPRedirect,
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

// TestClientStripsRefererOnCrossOriginRedirect pins the Referer leak: Go
// synthesizes a Referer header from the previous hop's URL — query string
// included — on every followed redirect, so an endpoint whose query carries
// credentials would hand them to the redirect target through the client's
// own Referer. The redirect policy must strip it before the hop goes out;
// without the strip the target here receives
// `Referer: <source>/mcp?api_key=referer-secret`.
func TestClientStripsRefererOnCrossOriginRedirect(t *testing.T) {
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

	// Wired through httpClientForServer so the endpoint URL carries the
	// secret in its query and origin-scoped header injection is in play.
	client := httpClientForServer(config.MCPServerConfig{
		URL:     source.URL + "/mcp?api_key=referer-secret",
		Headers: map[string]string{"Authorization": "Bearer secret"},
	})

	resp, err := client.Get(source.URL + "/mcp?api_key=referer-secret")
	if err != nil {
		t.Fatalf("follow redirect: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.ReadAll(resp.Body)

	if got := targetGot.Get("Referer"); got != "" {
		t.Fatalf("synthesized Referer leaked to redirect target: %q", got)
	}
	// Header scoping still guards the same hop.
	if got := targetGot.Get("Authorization"); got != "" {
		t.Fatalf("credential leaked to redirect target: Authorization=%q", got)
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
		CheckRedirect: checkMCPRedirect,
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

// TestCheckMCPRedirect pins the redirect policy: an HTTPS endpoint must
// never be followed to a plain-HTTP target, on any host. Same- or upgraded-
// scheme redirects stay allowed.
func TestCheckMCPRedirect(t *testing.T) {
	via := []*http.Request{{URL: mustParseURL(t, "https://mcp.example.invalid/mcp")}}

	downgrade, err := http.NewRequest(http.MethodGet, "http://mcp.example.invalid/mcp", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	err = checkMCPRedirect(downgrade, via)
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
	if err := checkMCPRedirect(crossHostHTTPS, via); err != nil {
		t.Fatalf("expected https to https redirect to be allowed, got %v", err)
	}
}

// TestCheckMCPRedirectCapsRedirectHops pins the hop limit Go's default
// CheckRedirect would enforce: a custom policy replaces it, so after ten
// hops the eleventh must be refused instead of letting a redirecting (or
// self-redirecting) endpoint walk the client forever.
func TestCheckMCPRedirectCapsRedirectHops(t *testing.T) {
	via := make([]*http.Request, maxRedirectHops)
	for i := range via {
		via[i] = &http.Request{URL: mustParseURL(t, "https://mcp.example.invalid/hop")}
	}

	next, err := http.NewRequest(http.MethodGet, "https://mcp.example.invalid/hop", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	err = checkMCPRedirect(next, via)
	if err == nil {
		t.Fatalf("expected the %dth redirect hop to be refused", maxRedirectHops+1)
	}
	if !strings.Contains(err.Error(), "stopped after 10 redirects") {
		t.Fatalf("expected the default redirect-cap message, got %q", err.Error())
	}

	// Nine prior hops are still within the cap.
	if err := checkMCPRedirect(next, via[:maxRedirectHops-1]); err != nil {
		t.Fatalf("expected 9 prior hops to still pass, got %v", err)
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
