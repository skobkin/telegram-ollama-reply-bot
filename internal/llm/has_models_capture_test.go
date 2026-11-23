package llm

import (
	"context"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"telegram-ollama-reply-bot/internal/config"
)

// dialTimeoutError is the error a dial that exceeds its Dialer.Timeout returns
// ("dial tcp ...: i/o timeout"). The net package makes it match
// context.DeadlineExceeded through Is although no request context expired.
func dialTimeoutError(t *testing.T) error {
	t.Helper()

	_, err := (&net.Dialer{Timeout: time.Nanosecond}).DialContext(context.Background(), "tcp", "192.0.2.1:443")
	if err == nil {
		t.Fatal("a 1ns dial must time out")
	}

	return err
}

// HasModelsFor only runs at startup, outside any request. A backend that does
// not answer (a dial timeout) is a real problem there, not an expired request,
// so it must reach Sentry even though the timeout matches
// context.DeadlineExceeded.
func TestHasModelsForReportsTransportTimeoutsToSentry(t *testing.T) {
	counter := bindSentryEventCounter(t)

	service := &Service{
		cfg: config.LLMConfig{
			Features: config.FeatureConfig{Chat: config.FeatureRouteConfig{Model: "gpt"}},
		},
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		backend: &stubBackend{name: config.LLMBackendOpenAICompat, listModelsErr: dialTimeoutError(t)},
	}

	if hasAll, _ := service.HasModelsFor(context.Background(), FeatureChat); hasAll {
		t.Fatal("a failing model list must fail the startup check")
	}
	if got := counter.events.Load(); got != 1 {
		t.Fatalf("a startup transport timeout produced %d Sentry event(s), want 1", got)
	}
}
