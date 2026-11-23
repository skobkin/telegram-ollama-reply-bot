package llm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/getsentry/sentry-go"

	"telegram-ollama-reply-bot/internal/config"
	"telegram-ollama-reply-bot/internal/support/errreport"
)

// sentryEventCounter is a sentry.Transport that only counts delivered events.
type sentryEventCounter struct {
	events atomic.Int32
}

func (c *sentryEventCounter) Configure(sentry.ClientOptions)        {}
func (c *sentryEventCounter) SendEvent(*sentry.Event)               { c.events.Add(1) }
func (c *sentryEventCounter) Flush(time.Duration) bool              { return true }
func (c *sentryEventCounter) FlushWithContext(context.Context) bool { return true }
func (c *sentryEventCounter) Close()                                {}

// bindSentryEventCounter routes the global Sentry hub to a counting transport
// for the duration of the test. It mutates process-wide state, so tests using
// it must not call t.Parallel.
func bindSentryEventCounter(t *testing.T) *sentryEventCounter {
	t.Helper()

	counter := &sentryEventCounter{}
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

// A prompt that fails to render because the request ended (deadline, shutdown)
// is not a defective template. The failure keeps the context sentinel
// matchable, so neither the service nor the capture sites downstream report
// it, in explicit and silent runs alike; an ordinary template error stays the
// bare, reported kind.
func TestTemplateFailureKeepsContextErrorsRecognisable(t *testing.T) {
	newService := func(renderErr error) *Service {
		return &Service{
			cfg:     config.LLMConfig{Features: config.FeatureConfig{Chat: config.FeatureRouteConfig{Model: "gpt"}}},
			prompts: &failingRendererStub{err: renderErr},
			logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		}
	}
	chat := func(service *Service, silent bool) error {
		_, err := service.BuildChatToolRequest(context.Background(), PromptScope{}, ChatReplyContext{
			UserMessage: TextMessage(RoleUser, "hello"),
		}, "", nil, "policy", nil, silent)

		return err
	}
	summarize := func(service *Service, silent bool) error {
		_, _, err := service.summarize(context.Background(), PromptScope{}, "text", "", silent)

		return err
	}

	for name, cause := range map[string]error{
		"deadline": fmt.Errorf("get global settings: %w", context.DeadlineExceeded),
		"canceled": fmt.Errorf("get global settings: %w", context.Canceled),
	} {
		for _, silent := range []bool{false, true} {
			for kind, run := range map[string]func(*Service, bool) error{"chat": chat, "summarize": summarize} {
				t.Run(fmt.Sprintf("%s/%s/silent=%v", name, kind, silent), func(t *testing.T) {
					counter := bindSentryEventCounter(t)

					err := run(newService(cause), silent)
					if !errors.Is(err, ErrTemplateProcessing) {
						t.Fatalf("expected ErrTemplateProcessing, got %v", err)
					}
					if !errreport.IsContextError(err) {
						t.Fatalf("the context cause must stay matchable for downstream capture sites: %v", err)
					}
					errreport.Capture(err) // what the callers do with the returned error
					if got := counter.events.Load(); got != 0 {
						t.Fatalf("an expired request produced %d Sentry event(s), want 0", got)
					}
				})
			}
		}
	}

	for _, silent := range []bool{false, true} {
		t.Run(fmt.Sprintf("ordinary/silent=%v", silent), func(t *testing.T) {
			counter := bindSentryEventCounter(t)

			err := chat(newService(errors.New("template parse exploded")), silent)
			if !errors.Is(err, ErrTemplateProcessing) || err.Error() != ErrTemplateProcessing.Error() {
				t.Fatalf("an ordinary template error must return the bare kind (no wrapping, no raw text), got %v", err)
			}
			if got := counter.events.Load(); got != 1 {
				t.Fatalf("a defective template must still be reported once, got %d events", got)
			}
		})
	}
}
