package transport

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"testing"

	"telegram-ollama-reply-bot/internal/config"
)

// TestLogNetworkRoutingIncludesSystemOneEndpoint pins the System One entry
// in the startup routing snapshot: the endpoint renders redacted (decision
// endpoint URLs can carry credentials) and the resolved proxy policy is
// reported alongside it.
func TestLogNetworkRoutingIncludesSystemOneEndpoint(t *testing.T) {
	cfg := &config.Config{}
	cfg.SystemOne.BaseURL = "http://operator:secret@so.internal:8090"

	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	LogNetworkRouting(logger, cfg)

	var record map[string]any
	if err := json.Unmarshal(buf.Bytes(), &record); err != nil {
		t.Fatalf("decode log record: %v", err)
	}

	baseURL, _ := record["systemone_base_url"].(string)
	if baseURL == "" || strings.Contains(baseURL, "secret") {
		t.Fatalf("systemone endpoint must be logged without credentials, got %q", baseURL)
	}
	if !strings.Contains(baseURL, "so.internal:8090") {
		t.Fatalf("systemone endpoint host must stay visible, got %q", baseURL)
	}
	proxy, _ := record["systemone_proxy"].(string)
	if proxy == "" {
		t.Fatalf("systemone proxy resolution must be reported")
	}
}

// TestLogNetworkRoutingUnsetSystemOneEndpoint pins the unset shape: an empty
// endpoint reads as unset rather than an empty or n/a URL.
func TestLogNetworkRoutingUnsetSystemOneEndpoint(t *testing.T) {
	cfg := &config.Config{}

	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	LogNetworkRouting(logger, cfg)

	if !strings.Contains(buf.String(), `"systemone_base_url":"unset"`) {
		t.Fatalf("empty endpoint must read as unset, got %s", buf.String())
	}
	if !strings.Contains(buf.String(), `"systemone_proxy":"n/a"`) {
		t.Fatalf("empty endpoint proxy must read as n/a, got %s", buf.String())
	}
}

func TestResolveProxyForURLReturnsDirectWhenProxyIsNotUsed(t *testing.T) {
	t.Parallel()

	got := resolveProxyForURL("https://api.telegram.org", func(*url.URL) (*url.URL, error) {
		return nil, nil
	})

	if got != "direct" {
		t.Fatalf("expected direct, got %q", got)
	}
}

func TestResolveProxyForURLRedactsCredentials(t *testing.T) {
	t.Parallel()

	got := resolveProxyForURL("https://api.telegram.org", func(*url.URL) (*url.URL, error) {
		return url.Parse("http://bot-user:secret@proxy.internal:8080")
	})

	proxyURL, err := url.Parse(got)
	if err != nil {
		t.Fatalf("unexpected invalid proxy url: %q", got)
	}
	if proxyURL.Scheme != "http" || proxyURL.Host != "proxy.internal:8080" || proxyURL.User.Username() != "bot-user" {
		t.Fatalf("unexpected proxy url: %q", got)
	}
	password, _ := proxyURL.User.Password()
	if password != "redacted" {
		t.Fatalf("expected redacted proxy password, got %q", password)
	}
}

func TestResolveProxyForURLRedactsParseErrorCredential(t *testing.T) {
	t.Parallel()

	// A malformed endpoint URL makes url.Parse quote the raw value —
	// credentials included — and the diagnostic must not carry it.
	leaky := "https://operator:secret@host.invalid:9999/pa%zz"
	got := resolveProxyForURL(leaky, func(*url.URL) (*url.URL, error) {
		t.Fatal("proxy resolution must not run for an unparseable URL")

		return nil, nil
	})

	if !strings.HasPrefix(got, "request_error:") {
		t.Fatalf("expected request_error classification, got %q", got)
	}

	if strings.Contains(got, "secret") || strings.Contains(got, "operator") {
		t.Fatalf("parse error leaked credentials: %q", got)
	}

	if !strings.Contains(got, "host.invalid") {
		t.Fatalf("parse error must keep the host diagnosable: %q", got)
	}
}

func TestResolveProxyForURLRedactsProxyErrorCredential(t *testing.T) {
	t.Parallel()

	// Proxy resolution failures can quote the proxy address with its
	// userinfo; the error text must be redacted, the classification kept.
	got := resolveProxyForURL("https://api.telegram.org", func(*url.URL) (*url.URL, error) {
		return nil, fmt.Errorf("invalid proxy address %q", "http://user:secret@proxy.internal:8080")
	})

	if !strings.HasPrefix(got, "proxy_error:") {
		t.Fatalf("expected proxy_error classification, got %q", got)
	}

	if strings.Contains(got, "secret") {
		t.Fatalf("proxy error leaked credentials: %q", got)
	}

	if !strings.Contains(got, "invalid proxy address") {
		t.Fatalf("proxy error lost its cause: %q", got)
	}
}

func TestRedactProxyValueReturnsSetForInvalidURL(t *testing.T) {
	got := redactProxyValue("proxy.internal:8080")
	if got != "set" {
		t.Fatalf("expected generic set marker, got %q", got)
	}
}

func TestRedactNoProxyValueReturnsValue(t *testing.T) {
	got := redactNoProxyValue("192.168.1.0/24,*.lan")
	if got != "192.168.1.0/24,*.lan" {
		t.Fatalf("unexpected no_proxy value: %q", got)
	}
}
