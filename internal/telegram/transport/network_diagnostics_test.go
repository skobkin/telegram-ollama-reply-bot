package transport

import (
	"net/url"
	"testing"
)

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
