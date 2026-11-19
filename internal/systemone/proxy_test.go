package systemone

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The shared HTTP client resolves proxy environment variables by design —
// deployment proxies and NO_PROXY exceptions must apply to decision traffic
// the same way they apply to every other outbound connection. These tests
// pin that behavior for both System One operations: inference and model
// discovery.
//
// Go's proxy resolution never routes loopback targets through a proxy, so
// the endpoint under test uses a non-loopback placeholder host and the
// recording proxy forwards to the real (loopback) backend.

// proxyTargetHost is the non-loopback placeholder the client points at; the
// proxy rewrites requests for it onto the real backend.
const proxyTargetHost = "so-under-test.internal"

// proxyBackend is the recording System One endpoint the proxy forwards to.
type proxyBackend struct {
	server *httptest.Server
	hits   atomic.Int64
	paths  chan string
}

func newProxyBackend(t *testing.T, body string) *proxyBackend {
	t.Helper()

	backend := &proxyBackend{paths: make(chan string, 8)}
	backend.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backend.hits.Add(1)
		backend.paths <- r.URL.Path
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(backend.server.Close)

	return backend
}

// recordingProxy is a minimal HTTP forward proxy: it counts the requests it
// receives and forwards each one to the given backend, copying the status
// and body back. Requests arrive with the absolute target URL.
func newRecordingProxy(t *testing.T, backend *proxyBackend) *recordingProxy {
	t.Helper()

	proxy := &recordingProxy{}
	direct := &http.Client{Transport: &http.Transport{Proxy: nil}}
	proxy.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxy.hits.Add(1)

		target := backend.server.URL + r.URL.Path
		if r.URL.RawQuery != "" {
			target += "?" + r.URL.RawQuery
		}
		// Forwarding a caller-controlled URL is the point of a proxy; this
		// one only ever rewrites onto the test backend in-process.
		out, err := http.NewRequestWithContext(r.Context(), r.Method, target, r.Body) //nolint:gosec // G704: deliberate test proxy
		if err != nil {
			http.Error(w, "proxy forward failed", http.StatusBadGateway)

			return
		}
		out.Header = r.Header.Clone()

		resp, err := direct.Do(out) //nolint:gosec // G704: same test-local rewrite
		if err != nil {
			http.Error(w, "proxy forward failed", http.StatusBadGateway)

			return
		}
		defer func() { _ = resp.Body.Close() }()

		for key, values := range resp.Header {
			for _, value := range values {
				w.Header().Add(key, value)
			}
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}))
	t.Cleanup(proxy.server.Close)

	return proxy
}

type recordingProxy struct {
	server *httptest.Server
	hits   atomic.Int64
}

// proxiedClient builds a client against the placeholder endpoint after
// pinning the proxy environment: httpclient.New reads the environment at
// construction, so the client must be built after t.Setenv. Every ambient
// proxy-policy variable is cleared first (both case variants — the
// resolution prefers the first non-empty), including REQUEST_METHOD: a
// nonempty value switches httpproxy into CGI mode, which refuses HTTP_PROXY
// outright, so an inherited CGI variable would break the is-proxied cases.
func proxiedClient(t *testing.T, env map[string]string) *Client {
	t.Helper()

	for _, key := range []string{"HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy", "NO_PROXY", "no_proxy", "REQUEST_METHOD"} {
		t.Setenv(key, "")
	}
	for key, value := range env {
		t.Setenv(key, value)
	}

	client, err := New(Config{
		BaseURL:          "http://" + proxyTargetHost + ":8090",
		Timeout:          5 * time.Second,
		MaxResponseBytes: 1 << 20,
		InputBudgetBytes: 96 << 10,
	}, nil)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}

	return client
}

func TestSystemOneTrafficFollowsProxyEnvironment(t *testing.T) {
	t.Run("evaluate is proxied", func(t *testing.T) {
		backend := newProxyBackend(t, referenceResponse)
		proxy := newRecordingProxy(t, backend)
		client := proxiedClient(t, map[string]string{"HTTP_PROXY": proxy.server.URL})

		_, err := client.Evaluate(context.Background(), Request{Model: "laya", State: "s", Questions: referenceQuestions()})
		if err != nil {
			t.Fatalf("evaluate: %v", err)
		}
		if backend.hits.Load() != 1 {
			t.Fatalf("backend must receive the request, got %d hits", backend.hits.Load())
		}
		if got := <-backend.paths; got != evaluatePath {
			t.Fatalf("unexpected backend path %q", got)
		}
		if proxy.hits.Load() != 1 {
			t.Fatalf("request must traverse the configured proxy, got %d proxy hits", proxy.hits.Load())
		}
	})

	t.Run("model discovery is proxied", func(t *testing.T) {
		backend := newProxyBackend(t, `{"object":"list","data":[{"id":"laya"}]}`)
		proxy := newRecordingProxy(t, backend)
		client := proxiedClient(t, map[string]string{"HTTP_PROXY": proxy.server.URL})

		models, err := client.ListModels(context.Background())
		if err != nil {
			t.Fatalf("list models: %v", err)
		}
		if len(models) != 1 || models[0] != "laya" {
			t.Fatalf("unexpected model list: %v", models)
		}
		if backend.hits.Load() != 1 {
			t.Fatalf("backend must receive the request, got %d hits", backend.hits.Load())
		}
		if got := <-backend.paths; got != modelsPath {
			t.Fatalf("unexpected backend path %q", got)
		}
		if proxy.hits.Load() != 1 {
			t.Fatalf("request must traverse the configured proxy, got %d proxy hits", proxy.hits.Load())
		}
	})
}

func TestSystemOneTrafficBypassesProxyWithinNoProxy(t *testing.T) {
	// The bypassed request dials the placeholder host directly, so no
	// backend is reachable — the proxy hit counter is the assertion.
	proxy := newRecordingProxy(t, newProxyBackend(t, referenceResponse))

	t.Run("evaluate bypasses the proxy", func(t *testing.T) {
		client := proxiedClient(t, map[string]string{
			"HTTP_PROXY": proxy.server.URL,
			"NO_PROXY":   proxyTargetHost,
		})

		// The bypassed request goes straight to the placeholder host, which
		// does not resolve: the failure itself proves no proxy answered.
		_, err := client.Evaluate(context.Background(), Request{Model: "laya", State: "s", Questions: referenceQuestions()})
		if err == nil || !strings.Contains(err.Error(), "unavailable") {
			t.Fatalf("a bypassed request must fail direct dialing, got %v", err)
		}
		if proxy.hits.Load() != 0 {
			t.Fatalf("NO_PROXY hosts must not traverse the proxy, got %d proxy hits", proxy.hits.Load())
		}
	})

	t.Run("model discovery bypasses the proxy", func(t *testing.T) {
		client := proxiedClient(t, map[string]string{
			"HTTP_PROXY": proxy.server.URL,
			"NO_PROXY":   proxyTargetHost,
		})

		_, err := client.ListModels(context.Background())
		if err == nil || !strings.Contains(err.Error(), "unavailable") {
			t.Fatalf("a bypassed request must fail direct dialing, got %v", err)
		}
		if proxy.hits.Load() != 0 {
			t.Fatalf("NO_PROXY hosts must not traverse the proxy, got %d proxy hits", proxy.hits.Load())
		}
	})
}
