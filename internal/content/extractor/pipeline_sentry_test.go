package extractor

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/getsentry/sentry-go"

	"telegram-ollama-reply-bot/internal/content/fetch"
)

// eventCounter is a sentry.Transport that only counts delivered events.
type eventCounter struct {
	events atomic.Int32
}

func (c *eventCounter) Configure(sentry.ClientOptions)        {}
func (c *eventCounter) SendEvent(*sentry.Event)               { c.events.Add(1) }
func (c *eventCounter) Flush(time.Duration) bool              { return true }
func (c *eventCounter) FlushWithContext(context.Context) bool { return true }
func (c *eventCounter) Close()                                {}

// bindEventCounter routes the global Sentry hub to a counting transport for
// the duration of the test. It mutates process-wide state, so tests using it
// must not call t.Parallel.
func bindEventCounter(t *testing.T) *eventCounter {
	t.Helper()

	counter := &eventCounter{}
	client, err := sentry.NewClient(sentry.ClientOptions{
		Dsn:       "https://publickey@sentry.example.com/1",
		Transport: counter,
	})
	if err != nil {
		t.Fatalf("create sentry client: %v", err)
	}

	hub := sentry.CurrentHub()
	previous := hub.Client()
	hub.BindClient(client)
	t.Cleanup(func() { hub.BindClient(previous) })

	return counter
}

// A failed extraction must reach Sentry from the pipeline only, and only when
// the failure is a real defect: policy blocks and context errors are expected
// outcomes that stay in the logs.
func TestPipelineFetchFailureSentryReporting(t *testing.T) {
	tests := []struct {
		name      string
		fetchErr  error
		wantEvent int32
	}{
		{"network failure is reported once", errors.New("fetch failed: connection reset"), 1},
		{"policy block is not reported", fetch.ErrBlocked, 0},
		{"deadline is not reported", context.DeadlineExceeded, 0},
		{"cancellation is not reported", context.Canceled, 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			counter := bindEventCounter(t)
			pipeline := NewPipeline(
				stubFetcher{err: tt.fetchErr},
				[]Backend{stubBackend{name: "never", content: cleanContent()}},
				testLogger(),
			)

			_, err := pipeline.GetArticleFromURL(context.Background(), "https://example.com/article")
			if !errors.Is(err, ErrExtractFailed) {
				t.Fatalf("expected ErrExtractFailed, got %v", err)
			}
			if got := counter.events.Load(); got != tt.wantEvent {
				t.Fatalf("sentry events = %d, want %d", got, tt.wantEvent)
			}
		})
	}
}

// Routine outcomes stay log-only: when every backend abstains (empty content)
// or fails without a captured cause of its own, the pipeline returns
// ErrExtractFailed but reports nothing — the comment on the all-failed return
// documents this as deliberate.
func TestPipelineAllBackendsAbstainingIsNotReported(t *testing.T) {
	counter := bindEventCounter(t)
	pipeline := NewPipeline(
		stubFetcher{},
		[]Backend{
			stubBackend{name: "first", content: Content{Empty: true}},
			stubBackend{name: "second", content: Content{Empty: true}},
		},
		testLogger(),
	)

	_, err := pipeline.GetArticleFromURL(context.Background(), "https://example.com/js-only")
	if !errors.Is(err, ErrExtractFailed) {
		t.Fatalf("expected ErrExtractFailed, got %v", err)
	}
	if got := counter.events.Load(); got != 0 {
		t.Fatalf("sentry events = %d, want 0 for an all-abstain page", got)
	}
}
