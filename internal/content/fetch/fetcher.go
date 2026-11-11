// Package fetch provides the shared, SSRF-hardened HTTP fetcher used to
// download web content for article extraction. It is the single place where
// user-supplied URLs are turned into fetched pages: URL validation, network
// policy (deny-by-default IP ranges and operator deny lists), redirect
// handling, response size limits, and character-set decoding all live here so
// extraction backends never perform their own HTTP requests.
//
// Security model:
//
//   - Direct connections validate the destination at the dial level: every IP
//     the host resolves to is checked against the policy before connecting,
//     which closes DNS-rebinding windows (a resolution that later flips, or a
//     record set mixing public and private addresses, is rejected).
//   - When an operator configures HTTP_PROXY/HTTPS_PROXY (NO_PROXY honored),
//     the connection to the proxy itself is exempt from the IP policy — proxy
//     endpoints are operator infrastructure and frequently live on private
//     networks. Destination protection then degrades to the request level:
//     scheme and deny-domain checks are enforced on every hop, while IP/CIDR
//     validation of the destination is delegated to the proxy. This is a
//     documented gap tracked in the issue tracker (proxied destination IP
//     validation).
//   - Redirects are re-validated per hop, and the hop budget fails the fetch
//     with an error instead of returning the last response.
package fetch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/net/html/charset"
	"golang.org/x/net/http/httpproxy"
)

// Defaults applied when an Options field is left at its zero value. The
// config layer seeds the same values as env-var defaults; these keep direct
// construction (tests, embedders) safe.
const (
	DefaultTimeout      = 10 * time.Second
	DefaultMaxBodyBytes = int64(2 << 20) // 2 MiB
	DefaultMaxRedirects = 5
	DefaultUserAgent    = "Mozilla/5.0 (X11; Linux x86_64; rv:140.0) Gecko/20100101 Firefox/140.0"
)

// Sentinel errors. Callers (and the LLM tool path) see only these wrapped,
// sanitized messages: the underlying causes — resolved addresses, redirect
// targets, response headers — stay in the logs.
var (
	// ErrBlocked is returned when the fetch policy refuses a URL, a resolved
	// address, or a redirect hop.
	ErrBlocked = errors.New("url blocked by fetch policy")
	// ErrBodyTooLarge is returned when the response body exceeds the
	// configured size limit.
	ErrBodyTooLarge = errors.New("response body exceeds size limit")
	// ErrUnsupported is returned for URLs the fetcher does not accept at all:
	// non-HTTP(S) schemes, missing hosts, or embedded credentials.
	ErrUnsupported = errors.New("unsupported url")
	// ErrRedirectLimit is returned when a fetch exceeds the configured
	// redirect budget; the fetch fails, the last response is not returned.
	ErrRedirectLimit = errors.New("too many redirects")
)

// Options configures a Fetcher. Zero values fall back to the package
// defaults; DenyCIDRs entries must parse as CIDRs or New returns an error.
type Options struct {
	Timeout      time.Duration
	MaxBodyBytes int64
	MaxRedirects int
	UserAgent    string
	DenyDomains  []string
	DenyCIDRs    []string
}

// Page is the result of a successful fetch: the UTF-8 decoded document plus
// the context needed to interpret it.
type Page struct {
	// URL is the final URL after following redirects; relative links and
	// metadata on the page resolve against it.
	URL string
	// ContentType is the raw Content-Type header of the final response.
	ContentType string
	// Body is the (size-capped) document, decoded to UTF-8.
	Body []byte
}

// ipResolver is the DNS boundary the fetcher depends on; *net.Resolver
// satisfies it, tests can inject stubs.
type ipResolver interface {
	LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error)
}

// Fetcher downloads pages once per request for the extraction pipeline.
// Construct with New; the zero value is not usable.
type Fetcher struct {
	client     *http.Client
	policy     *policy
	resolver   ipResolver
	proxyAddrs map[string]struct{}

	userAgent    string
	timeout      time.Duration
	maxBodyBytes int64
	maxRedirects int

	logger *slog.Logger
}

// New builds a Fetcher from options. It fails when operator-provided CIDRs
// cannot parse; everything else falls back to defaults. A nil logger is
// replaced with a package-scoped default.
func New(opts Options, logger *slog.Logger) (*Fetcher, error) {
	if logger == nil {
		logger = slog.Default().With("pkg", "content/fetch")
	}

	deniedCIDRs, err := parseCIDRs(opts.DenyCIDRs)
	if err != nil {
		return nil, fmt.Errorf("content.fetch.deny_cidrs (CONTENT__FETCH__DENY_CIDRS): %w", err)
	}

	denyDomains := make([]string, 0, len(opts.DenyDomains))
	for _, d := range opts.DenyDomains {
		if d = strings.TrimSpace(d); d != "" {
			denyDomains = append(denyDomains, d)
		}
	}

	f := &Fetcher{
		policy: &policy{
			deniedRanges: append(defaultDeniedRanges(), deniedCIDRs...),
			denyDomains:  denyDomains,
		},
		resolver:     net.DefaultResolver,
		userAgent:    firstNonZero(opts.UserAgent, DefaultUserAgent),
		timeout:      firstNonZero(opts.Timeout, DefaultTimeout),
		maxBodyBytes: firstNonZero(opts.MaxBodyBytes, DefaultMaxBodyBytes),
		maxRedirects: firstNonZero(opts.MaxRedirects, DefaultMaxRedirects),
		logger:       logger,
	}

	proxyConfig := httpproxy.FromEnvironment()
	proxyFunc := proxyConfig.ProxyFunc()

	// Proxy endpoints are exempt from the IP policy: the transport dials
	// them instead of the destination, and operators legitimately point
	// HTTP_PROXY at internal infrastructure. The set is exact (host:port of
	// the configured proxies); anything else dialed goes through the
	// validating path in dialContext.
	proxyAddrs := make(map[string]struct{}, 2)
	for _, raw := range []string{proxyConfig.HTTPProxy, proxyConfig.HTTPSProxy} {
		if strings.TrimSpace(raw) == "" {
			continue
		}

		proxyURL, parseErr := url.Parse(raw)
		if parseErr != nil || proxyURL.Host == "" {
			logger.Debug("skipping unparseable proxy configuration", "error", parseErr)

			continue
		}

		proxyAddrs[normalizeHostPort(proxyURL)] = struct{}{}

		// Startup diagnostics: the address only, credentials redacted.
		safe := *proxyURL
		safe.User = nil
		logger.Info("content fetch proxy configured", "proxy", safe.String())
	}
	if len(proxyAddrs) == 0 {
		logger.Info("content fetch proxy configured", "proxy", "none")
	}
	f.proxyAddrs = proxyAddrs

	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = func(req *http.Request) (*url.URL, error) {
		return proxyFunc(req.URL)
	}
	transport.DialContext = f.dialContext

	f.client = &http.Client{
		Transport:     transport,
		Timeout:       f.timeout,
		CheckRedirect: f.checkRedirect,
	}

	return f, nil
}

// Fetch downloads rawURL once and returns the decoded page. Errors are
// sanitized sentinels (ErrUnsupported, ErrBlocked, ErrBodyTooLarge,
// ErrRedirectLimit) or context errors; network detail stays in the logs.
func (f *Fetcher) Fetch(ctx context.Context, rawURL string) (*Page, error) {
	parsed, err := validateURLShape(rawURL)
	if err != nil {
		f.logger.DebugContext(ctx, "url rejected", "error", err)

		return nil, err
	}
	if err := f.checkHopURL(parsed); err != nil {
		return nil, err
	}
	host := parsed.Hostname()

	f.logger.InfoContext(ctx, "fetching page", "host", host)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		f.logger.Debug("request construction failed", "host", host, "error", err)

		return nil, fmt.Errorf("%w: malformed request url", ErrUnsupported)
	}
	req.Header.Set("User-Agent", f.userAgent)
	req.Header.Set("Accept", "text/html, application/xhtml+xml")

	resp, err := f.client.Do(req)
	if err != nil {
		f.logger.Debug("http request failed", "host", host, "error", err)

		return nil, sanitizeTransportError(err)
	}
	defer func() { _ = resp.Body.Close() }()

	// resp.Request.URL is the destination of the last hop (post-redirect);
	// it is what relative links and metadata on the page resolve against.
	finalURL := ""
	if resp.Request != nil && resp.Request.URL != nil {
		finalURL = resp.Request.URL.String()
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		f.logger.Debug("unexpected status", "host", host, "status", resp.Status)

		return nil, fmt.Errorf("fetch failed: unexpected status %s", http.StatusText(resp.StatusCode))
	}

	contentType := resp.Header.Get("Content-Type")
	if !isHTMLContentType(contentType) {
		f.logger.Debug("unsupported content type", "host", host, "content_type", contentType)

		return nil, fmt.Errorf("%w: response is not an html document", ErrUnsupported)
	}

	// Decode to UTF-8 first (label from headers or document sniffing), then
	// cap the decoded size. The +1 probe distinguishes "exactly at the cap"
	// from "over it".
	decoded, err := charset.NewReader(resp.Body, contentType)
	if err != nil {
		f.logger.Debug("charset detection failed", "host", host, "error", err)

		return nil, fmt.Errorf("fetch failed: charset decoding error")
	}

	body, err := f.readLimited(decoded)
	if err != nil {
		f.logger.Debug("body read failed", "host", host, "error", err)

		return nil, err
	}

	f.logger.InfoContext(ctx, "page fetched", "host", host, "bytes", len(body))

	return &Page{
		URL:         finalURL,
		ContentType: contentType,
		Body:        body,
	}, nil
}

// validateURLShape is the pre-flight parse of the caller-supplied URL:
// requestable shape only (it parses, HTTP/HTTPS scheme, no credentials, a
// host present). Policy checks live in checkHopURL; resolved addresses are
// validated separately at dial time.
func validateURLShape(rawURL string) (*url.URL, error) {
	parsed, err := url.ParseRequestURI(rawURL)
	if err != nil {
		return nil, fmt.Errorf("%w: url does not parse", ErrUnsupported)
	}

	switch strings.ToLower(parsed.Scheme) {
	case "http", "https":
		// Allowed.
	default:
		return nil, fmt.Errorf("%w: scheme %q is not supported", ErrUnsupported, parsed.Scheme)
	}

	if parsed.User != nil {
		return nil, fmt.Errorf("%w: embedded credentials are not allowed", ErrUnsupported)
	}
	if parsed.Host == "" {
		return nil, fmt.Errorf("%w: url has no host", ErrUnsupported)
	}

	return parsed, nil
}

// checkHopURL validates one request destination (initial URL or redirect
// hop) against the policy: shape rules plus the hostname deny list. It is
// the URL-level half of the policy; resolved IP addresses are gated in
// dialContext. Errors are sanitized sentinels — the reason for a refusal
// must not leak target details to callers.
func (f *Fetcher) checkHopURL(u *url.URL) error {
	if u == nil {
		return fmt.Errorf("%w: empty url", ErrUnsupported)
	}

	if _, err := validateURLShape(u.String()); err != nil {
		return err
	}

	if !f.policy.allowedHostname(u.Hostname()) {
		return ErrBlocked
	}

	return nil
}

// checkRedirect is the http.Client redirect policy: re-validate every hop
// against the same rules as the initial URL, and fail the fetch once more
// than maxRedirects redirects would be followed — the last response is never
// quietly returned. via holds the already-followed requests, so len(via) ==
// maxRedirects is still the legitimate destination request after the final
// allowed redirect.
func (f *Fetcher) checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) > f.maxRedirects {
		return fmt.Errorf("%w (max %d)", ErrRedirectLimit, f.maxRedirects)
	}

	return f.checkHopURL(req.URL)
}

// dialContext is the transport's DialContext. Addresses matching a configured
// proxy are dialed as-is (operator infrastructure, see package docs);
// everything else must resolve to — or literally be — an allowed IP before
// any connection is made.
//
// The happy-eyeballs race of net.Dialer is traded for sequential attempts
// through the resolved addresses: policy validation must gate every
// candidate, and first-success-wins keeps the decision point in one place.
func (f *Fetcher) dialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("fetch failed: malformed dial address")
	}

	if _, isProxy := f.proxyAddrs[addr]; isProxy {
		var d net.Dialer

		return d.DialContext(ctx, network, addr)
	}

	if ip := net.ParseIP(host); ip != nil {
		// Literal IP destination: validated here, at dial time, so an
		// embedded private address is stopped regardless of earlier checks.
		if !f.policy.allowedIP(ip) {
			return nil, ErrBlocked
		}

		var d net.Dialer

		return d.DialContext(ctx, network, addr)
	}

	// Hostname: resolve it here instead of letting the dialer do it, so
	// every returned address can be checked — fail closed on a record set
	// that mixes public and private addresses (the DNS-rebinding case).
	addrs, err := f.resolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("fetch failed: dns resolution error")
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("fetch failed: dns resolution returned no addresses")
	}

	targets := make([]string, 0, len(addrs))
	for _, candidate := range addrs {
		if !f.policy.allowedIP(candidate.IP) {
			return nil, ErrBlocked
		}
		targets = append(targets, net.JoinHostPort(candidate.IP.String(), port))
	}

	var firstErr error
	for _, target := range targets {
		var d net.Dialer
		conn, dialErr := d.DialContext(ctx, network, target)
		if dialErr == nil {
			return conn, nil
		}
		if firstErr == nil {
			firstErr = dialErr
		}
		// A cancelled or expired context ends the loop: the request is
		// already gone.
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}

	return nil, firstErr
}

// readLimited reads at most maxBodyBytes from r, failing with
// ErrBodyTooLarge when the stream is longer. Context cancellation and
// deadline errors pass through unchanged so callers can react to them.
func (f *Fetcher) readLimited(r io.Reader) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r, f.maxBodyBytes+1))
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}

		return nil, fmt.Errorf("fetch failed: body read error")
	}
	if int64(len(body)) > f.maxBodyBytes {
		return nil, ErrBodyTooLarge
	}

	return body, nil
}

// sanitizeTransportError folds transport-layer failures into caller-safe
// errors: policy sentinels and context errors pass through unchanged, and
// everything else loses its wrapped detail (which can contain resolved
// addresses and header material) — the cause stays in the debug log.
func sanitizeTransportError(err error) error {
	switch {
	case errors.Is(err, ErrBlocked),
		errors.Is(err, ErrRedirectLimit),
		errors.Is(err, context.Canceled),
		errors.Is(err, context.DeadlineExceeded):
		return err
	}

	var urlErr *url.Error
	if errors.As(err, &urlErr) && urlErr.Err != nil {
		err = urlErr.Err
	}

	return fmt.Errorf("fetch failed: %w", err)
}

// isHTMLContentType reports whether a Content-Type header value names an
// HTML document type. A missing or malformed header is not HTML.
func isHTMLContentType(contentType string) bool {
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		mediaType = strings.ToLower(strings.TrimSpace(strings.SplitN(contentType, ";", 2)[0]))
	}

	switch mediaType {
	case "text/html", "application/xhtml+xml":
		return true
	default:
		return false
	}
}

// normalizeHostPort renders a proxy URL as the host:port the transport will
// actually dial, filling in the scheme's default port when unset.
func normalizeHostPort(u *url.URL) string {
	if _, _, err := net.SplitHostPort(u.Host); err == nil {
		return u.Host
	}

	port := "80"
	switch strings.ToLower(u.Scheme) {
	case "https":
		port = "443"
	case "socks5", "socks5h":
		port = "1080"
	}

	return net.JoinHostPort(u.Hostname(), port)
}

func firstNonZero[T ~int | ~int64 | string](value, fallback T) T {
	var zero T
	if value == zero {
		return fallback
	}

	return value
}
