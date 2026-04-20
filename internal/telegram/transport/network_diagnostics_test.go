package transport

import (
	"net/http"
	"net/url"
	"testing"
)

func TestResolveProxyForURLReturnsDirectWhenProxyIsNotUsed(t *testing.T) {
	t.Parallel()

	got := resolveProxyForURL("https://api.telegram.org", func(*http.Request) (*url.URL, error) {
		return nil, nil
	})

	if got != "direct" {
		t.Fatalf("expected direct, got %q", got)
	}
}

func TestResolveProxyForURLRedactsCredentials(t *testing.T) {
	t.Parallel()

	got := resolveProxyForURL("https://api.telegram.org", func(*http.Request) (*url.URL, error) {
		return url.Parse("http://bot-user:secret@proxy.internal:8080")
	})

	if got != "http://bot-user:redacted@proxy.internal:8080" {
		t.Fatalf("unexpected proxy url: %q", got)
	}
}

func TestRedactProxyEnvReturnsSetForInvalidURL(t *testing.T) {
	t.Setenv("HTTP_PROXY", "proxy.internal:8080")

	got := redactProxyEnv("HTTP_PROXY")
	if got != "set" {
		t.Fatalf("expected generic set marker, got %q", got)
	}
}

func TestRedactNoProxyEnvReturnsValue(t *testing.T) {
	t.Setenv("NO_PROXY", "192.168.1.0/24,*.lan")

	got := redactNoProxyEnv("NO_PROXY")
	if got != "192.168.1.0/24,*.lan" {
		t.Fatalf("unexpected no_proxy value: %q", got)
	}
}
