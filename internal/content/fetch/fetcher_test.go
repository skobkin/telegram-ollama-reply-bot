package fetch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// newTestFetcher builds a fetcher whose policy permits the loopback addresses
// httptest servers live on, while keeping everything else deny-by-default.
// The environment proxy is disabled at the transport level so direct-dial
// tests stay deterministic regardless of the developer's environment; the
// proxy tests build their own fetcher with explicit proxy settings.
func newTestFetcher(t *testing.T, mutate func(opts *Options, f *Fetcher)) *Fetcher {
	t.Helper()

	opts := Options{Timeout: 5 * time.Second, MaxBodyBytes: 1 << 20, MaxRedirects: 5}
	f, err := New(opts, testLogger())
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	// Replace the policy with an equivalent one that does not block loopback
	// (in-package access): user-denied test ranges still apply.
	f.policy = &policy{
		deniedRanges: mustParseCIDRs(t, []string{
			"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "169.254.0.0/16",
			"100.64.0.0/10", "224.0.0.0/4", "fc00::/7", "fe80::/10", "ff00::/8",
		}),
	}

	// Pin the proxy decision to "none": environment proxies must not
	// influence direct-dial tests.
	transportProxy(f, nil)
	f.proxyAddrs = map[string]struct{}{}

	if mutate != nil {
		mutate(&opts, f)
	}

	return f
}

// transportProxy overrides the fetcher transport's proxy function.
func transportProxy(f *Fetcher, proxy func(*http.Request) (*url.URL, error)) {
	f.client.Transport.(*http.Transport).Proxy = proxy
}

func mustParseCIDRs(t *testing.T, raw []string) []*net.IPNet {
	t.Helper()

	ranges, err := parseCIDRs(raw)
	if err != nil {
		t.Fatalf("parseCIDRs returned error: %v", err)
	}

	return ranges
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// stubResolver resolves every configured host to canned addresses, never
// touching the network.
type stubResolver struct {
	hosts map[string][]net.IPAddr
}

func (r *stubResolver) LookupIPAddr(_ context.Context, host string) ([]net.IPAddr, error) {
	if addrs, ok := r.hosts[host]; ok {
		return addrs, nil
	}

	return nil, fmt.Errorf("no such host %s", host)
}

func TestFetchHappyPath(t *testing.T) {
	t.Parallel()

	var seenUA, seenAccept string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenUA = r.Header.Get("User-Agent")
		seenAccept = r.Header.Get("Accept")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, "<html><body><p>hello</p></body></html>")
	}))
	defer server.Close()

	f := newTestFetcher(t, func(_ *Options, f *Fetcher) {
		f.userAgent = "test-agent/1.0"
	})

	page, err := f.Fetch(context.Background(), server.URL)
	if err != nil {
		t.Fatalf("Fetch returned error: %v", err)
	}

	if page.URL != server.URL {
		t.Fatalf("unexpected final url: %q", page.URL)
	}
	if seenUA != "test-agent/1.0" {
		t.Fatalf("custom user agent not sent, got %q", seenUA)
	}
	if !strings.Contains(seenAccept, "text/html") {
		t.Fatalf("accept header missing text/html, got %q", seenAccept)
	}
	if !strings.Contains(string(page.Body), "hello") {
		t.Fatalf("unexpected body: %q", page.Body)
	}
}

func TestFetchBlocksLoopbackByDefaultPolicy(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {}))
	defer server.Close()

	// Real fetcher (not newTestFetcher): the default policy must block the
	// loopback destination at dial time.
	f, err := New(Options{}, testLogger())
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	_, err = f.Fetch(context.Background(), server.URL)
	if !errors.Is(err, ErrBlocked) {
		t.Fatalf("expected ErrBlocked for loopback destination, got %v", err)
	}
}

func TestFetchResolvesPrivateIP(t *testing.T) {
	t.Parallel()

	f := newTestFetcher(t, func(_ *Options, f *Fetcher) {
		f.resolver = &stubResolver{hosts: map[string][]net.IPAddr{
			"private.example": {net.IPAddr{IP: net.ParseIP("10.1.2.3")}},
		}}
	})

	_, err := f.Fetch(context.Background(), "http://private.example/page")
	if !errors.Is(err, ErrBlocked) {
		t.Fatalf("expected ErrBlocked for private resolution, got %v", err)
	}
}

func TestFetchRejectsMixedResolution(t *testing.T) {
	t.Parallel()

	// A record set mixing public and private addresses must fail closed —
	// this is the DNS-rebinding shape.
	f := newTestFetcher(t, func(_ *Options, f *Fetcher) {
		f.resolver = &stubResolver{hosts: map[string][]net.IPAddr{
			"rebinding.example": {
				net.IPAddr{IP: net.ParseIP("93.184.216.34")},
				net.IPAddr{IP: net.ParseIP("10.0.0.1")},
			},
		}}
	})

	_, err := f.Fetch(context.Background(), "http://rebinding.example/page")
	if !errors.Is(err, ErrBlocked) {
		t.Fatalf("expected ErrBlocked for mixed public/private resolution, got %v", err)
	}
}

func TestFetchBlocksPrivateRedirectTarget(t *testing.T) {
	t.Parallel()

	var target string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, target, http.StatusFound)

			return
		}
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, "<html><body>landed</body></html>")
	}))
	defer server.Close()

	f := newTestFetcher(t, func(_ *Options, f *Fetcher) {
		f.resolver = &stubResolver{hosts: map[string][]net.IPAddr{
			"private.example": {net.IPAddr{IP: net.ParseIP("192.168.5.5")}},
		}}
	})

	// Redirect to a hostname that resolves private: the hop is validated by
	// the redirect policy and then again at dial time.
	target = "http://private.example/landing"

	_, err := f.Fetch(context.Background(), server.URL+"/redirect")
	if !errors.Is(err, ErrBlocked) {
		t.Fatalf("expected ErrBlocked for private redirect target, got %v", err)
	}
}

func TestFetchFollowsAllowedRedirects(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		switch r.URL.Path {
		case "/final":
			_, _ = io.WriteString(w, "<html><body>landed</body></html>")
		case "/hop3":
			http.Redirect(w, r, "/final", http.StatusFound)
		case "/hop2":
			http.Redirect(w, r, "/hop3", http.StatusFound)
		case "/hop1":
			http.Redirect(w, r, "/hop2", http.StatusFound)
		}
	}))
	defer server.Close()

	f := newTestFetcher(t, nil)

	page, err := f.Fetch(context.Background(), server.URL+"/hop1")
	if err != nil {
		t.Fatalf("Fetch returned error: %v", err)
	}
	if !strings.Contains(string(page.Body), "landed") {
		t.Fatalf("unexpected body: %q", page.Body)
	}
	if !strings.HasSuffix(page.URL, "/final") {
		t.Fatalf("final url should be the redirect destination, got %q", page.URL)
	}
}

func TestFetchRedirectChainLimit(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		switch r.URL.Path {
		case "/0":
			_, _ = io.WriteString(w, "<html><body>landed</body></html>")
		default:
			var next int
			_, _ = fmt.Sscanf(r.URL.Path, "/%d", &next)
			http.Redirect(w, r, fmt.Sprintf("/%d", next-1), http.StatusFound)
		}
	}))
	defer server.Close()

	f := newTestFetcher(t, func(_ *Options, f *Fetcher) {
		f.maxRedirects = 5
	})

	// "/5" issues exactly 5 redirects (/5 -> /4 -> ... -> /0): allowed.
	if _, err := f.Fetch(context.Background(), server.URL+"/5"); err != nil {
		t.Fatalf("chain of exactly maxRedirects redirects should succeed, got %v", err)
	}

	// "/6" issues 6 redirects: the fetch must fail, not return the last page.
	_, err := f.Fetch(context.Background(), server.URL+"/6")
	if !errors.Is(err, ErrRedirectLimit) {
		t.Fatalf("expected ErrRedirectLimit for over-budget chain, got %v", err)
	}
}

func TestFetchRejectsUnsupportedURLs(t *testing.T) {
	t.Parallel()

	f := newTestFetcher(t, nil)

	cases := []struct {
		name string
		url  string
		want error
	}{
		{"file scheme", "file:///etc/passwd", ErrUnsupported},
		{"ftp scheme", "ftp://example.com/file", ErrUnsupported},
		{"no host", "http://", ErrUnsupported},
		{"credentials", "http://user:pass@example.com/page", ErrUnsupported},
		{"garbage", "http://%zz", ErrUnsupported},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := f.Fetch(context.Background(), tc.url)
			if !errors.Is(err, tc.want) {
				t.Fatalf("expected %v, got %v", tc.want, err)
			}
		})
	}
}

func TestFetchRejectsNonHTMLContent(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"json": true}`)
	}))
	defer server.Close()

	f := newTestFetcher(t, nil)

	_, err := f.Fetch(context.Background(), server.URL)
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("expected ErrUnsupported for json response, got %v", err)
	}
}

func TestFetchBodyTooLarge(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, strings.Repeat("a", 4096))
	}))
	defer server.Close()

	f := newTestFetcher(t, func(_ *Options, f *Fetcher) {
		f.maxBodyBytes = 1024
	})

	_, err := f.Fetch(context.Background(), server.URL)
	if !errors.Is(err, ErrBodyTooLarge) {
		t.Fatalf("expected ErrBodyTooLarge, got %v", err)
	}
}

func TestFetchBodyExactlyAtLimit(t *testing.T) {
	t.Parallel()

	const bodySize = 2048
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, strings.Repeat("a", bodySize))
	}))
	defer server.Close()

	f := newTestFetcher(t, func(_ *Options, f *Fetcher) {
		f.maxBodyBytes = bodySize
	})

	page, err := f.Fetch(context.Background(), server.URL)
	if err != nil {
		t.Fatalf("expected fetch at exact limit to succeed, got %v", err)
	}
	if len(page.Body) != bodySize {
		t.Fatalf("unexpected body size %d, want %d", len(page.Body), bodySize)
	}
}

func TestFetchDecodesWindows1252(t *testing.T) {
	t.Parallel()

	// 0xE9 is "é" in windows-1252.
	body := append(
		[]byte("<html><body><p>caf"),
		0xE9,
	)
	body = append(body, []byte(" au lait</p></body></html>")...)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=windows-1252")
		_, _ = w.Write(body)
	}))
	defer server.Close()

	f := newTestFetcher(t, nil)

	page, err := f.Fetch(context.Background(), server.URL)
	if err != nil {
		t.Fatalf("Fetch returned error: %v", err)
	}

	if !strings.Contains(string(page.Body), "café au lait") {
		t.Fatalf("expected decoded utf-8 body, got %q", page.Body)
	}
}

func TestFetchTimesOut(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		<-release
	}))
	defer server.Close()
	defer close(release)

	f, err := New(Options{Timeout: 100 * time.Millisecond}, testLogger())
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	f.policy = &policy{} // permit loopback; deny nothing

	_, err = f.Fetch(context.Background(), server.URL)
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
}

func TestFetchContextCanceled(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, strings.Repeat("a", 8192))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-release
	}))
	defer server.Close()
	defer close(release)

	f := newTestFetcher(t, nil)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	_, err := f.Fetch(ctx, server.URL)
	if err == nil {
		t.Fatal("expected cancellation error, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

func TestFetchProxyRouting(t *testing.T) {
	// Not parallel: mutates proxy environment variables.

	var proxyMu sync.Mutex
	proxySawHost := ""
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A forward proxy receives absolute-form URLs for plain HTTP.
		proxyMu.Lock()
		proxySawHost = r.URL.Host
		proxyMu.Unlock()

		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, "<html><body>via proxy</body></html>")
	}))
	defer proxy.Close()

	// Env must be set before New reads it.
	t.Setenv("HTTP_PROXY", proxy.URL)

	f, err := New(Options{Timeout: 5 * time.Second}, testLogger())
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	// Permit the loopback destination host check for the non-proxy policy
	// while keeping the rest deny-by-default: the loopback proxy endpoint is
	// exempt at the dial level, exactly as in production.
	f.policy = &policy{
		deniedRanges: mustParseCIDRs(t, []string{
			"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "169.254.0.0/16",
			"100.64.0.0/10", "224.0.0.0/4", "fc00::/7", "fe80::/10", "ff00::/8",
		}),
	}

	page, err := f.Fetch(context.Background(), "http://public.example/page")
	if err != nil {
		t.Fatalf("Fetch through proxy returned error: %v", err)
	}

	proxyMu.Lock()
	saw := proxySawHost
	proxyMu.Unlock()

	if saw != "public.example" {
		t.Fatalf("proxy did not observe absolute-form request for public.example, got %q", saw)
	}
	if !strings.Contains(string(page.Body), "via proxy") {
		t.Fatalf("unexpected body: %q", page.Body)
	}
}

func TestFetchProxyDenyDomainStillEnforced(t *testing.T) {
	// Not parallel: mutates proxy environment variables.

	proxy := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		t.Errorf("request for denied domain reached the proxy: %s", r.URL.Host)
	}))
	defer proxy.Close()

	t.Setenv("HTTP_PROXY", proxy.URL)

	f, err := New(Options{}, testLogger())
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	f.policy = &policy{
		deniedRanges: mustParseCIDRs(t, []string{"10.0.0.0/8"}),
		denyDomains:  []string{"denied.example"},
	}

	_, err = f.Fetch(context.Background(), "http://denied.example/page")
	if !errors.Is(err, ErrBlocked) {
		t.Fatalf("expected ErrBlocked for denied domain behind proxy, got %v", err)
	}
}

func TestFetchNoProxyRoutesDirectAndKeepsPolicy(t *testing.T) {
	// Not parallel: mutates proxy environment variables.

	proxy := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		t.Errorf("NO_PROXY destination reached the proxy: %s", r.URL.Host)
	}))
	defer proxy.Close()

	t.Setenv("HTTP_PROXY", proxy.URL)
	t.Setenv("NO_PROXY", "private.example")

	f, err := New(Options{}, testLogger())
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	f.policy = &policy{
		deniedRanges: mustParseCIDRs(t, []string{
			"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "169.254.0.0/16",
			"100.64.0.0/10", "224.0.0.0/4", "fc00::/7", "fe80::/10", "ff00::/8",
		}),
	}
	f.resolver = &stubResolver{hosts: map[string][]net.IPAddr{
		"private.example": {net.IPAddr{IP: net.ParseIP("10.9.9.9")}},
	}}

	// NO_PROXY sends the destination to the direct-dial path, where the IP
	// policy applies: a private resolution is still blocked.
	_, err = f.Fetch(context.Background(), "http://private.example/page")
	if !errors.Is(err, ErrBlocked) {
		t.Fatalf("expected ErrBlocked for private destination on the direct path, got %v", err)
	}
}
