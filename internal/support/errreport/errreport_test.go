package errreport

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/getsentry/sentry-go"

	"telegram-ollama-reply-bot/internal/support/safeerr"
)

var (
	errBackend = errors.New("backend failed")
	errTimeout = errors.New("request timeout")
)

func TestIsContextError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"plain", errBackend, false},
		{"canceled", context.Canceled, true},
		{"deadline", context.DeadlineExceeded, true},
		{"wrapped deadline", fmt.Errorf("get global settings: %w", context.DeadlineExceeded), true},
		{"joined cancel", errors.Join(errBackend, context.Canceled), true},
		{"request timeout sentinel joined with deadline", errors.Join(errTimeout, context.DeadlineExceeded), true},
		{"safeerr deadline", safeerr.WrapTransport(errBackend, context.DeadlineExceeded), true},
		{"safeerr canceled", safeerr.WrapTransport(errBackend, context.Canceled), true},
		{"safeerr other failure", safeerr.WrapTransport(errBackend, errors.New("boom")), false},
		{"joined without context", errors.Join(errBackend, errTimeout), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := IsContextError(tt.err); got != tt.want {
				t.Fatalf("IsContextError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// countingTransport counts events the SDK hands to the transport.
type countingTransport struct {
	events atomic.Int32
}

func (c *countingTransport) Configure(sentry.ClientOptions) {}
func (c *countingTransport) SendEvent(*sentry.Event)        { c.events.Add(1) }
func (c *countingTransport) Flush(time.Duration) bool       { return true }
func (c *countingTransport) FlushWithContext(context.Context) bool {
	return true
}
func (c *countingTransport) Close() {}

func newCountingHub(t *testing.T) (*sentry.Hub, *countingTransport) {
	t.Helper()

	transport := &countingTransport{}
	client, err := sentry.NewClient(sentry.ClientOptions{
		Dsn:       "https://publickey@sentry.example.com/1",
		Transport: transport,
	})
	if err != nil {
		t.Fatalf("create sentry client: %v", err)
	}

	return sentry.NewHub(client, sentry.NewScope()), transport
}

func TestCaptureWithReportsOnlyNonContextErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want int32
	}{
		{"nil", nil, 0},
		{"deadline", context.DeadlineExceeded, 0},
		{"canceled", fmt.Errorf("typing: %w", context.Canceled), 0},
		{"safeerr deadline", safeerr.WrapTransport(errBackend, context.DeadlineExceeded), 0},
		{"real failure", errBackend, 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			hub, transport := newCountingHub(t)
			captureWith(hub, tt.err)

			if got := transport.events.Load(); got != tt.want {
				t.Fatalf("events sent = %d, want %d", got, tt.want)
			}
		})
	}
}
