package transport

import (
	"bytes"
	"encoding/json"
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
