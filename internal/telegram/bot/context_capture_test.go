package bot

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/getsentry/sentry-go"

	"telegram-ollama-reply-bot/internal/adminconfig"
	"telegram-ollama-reply-bot/internal/config"
	"telegram-ollama-reply-bot/internal/content/extractor"
	"telegram-ollama-reply-bot/internal/llm"
	"telegram-ollama-reply-bot/internal/llmcontext"
	"telegram-ollama-reply-bot/internal/state"
	"telegram-ollama-reply-bot/internal/support/markdown"
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

// summarizerFunc adapts a function to the handler's summarizer dependency.
type summarizerFunc func(ctx context.Context) error

func (f summarizerFunc) Summarize(ctx context.Context, _ llm.PromptScope, _, _ string) (string, *llm.TokenUsage, error) {
	return "", nil, f(ctx)
}

func (f summarizerFunc) SummarizeSilent(ctx context.Context, scope llm.PromptScope, text, instructions string) (string, *llm.TokenUsage, error) {
	return f.Summarize(ctx, scope, text, instructions)
}

// Shutdown cancels the in-flight handler context (telego cancels it on
// BotHandler.Stop). A /summarize aborted that way is not a defect, so it must
// not reach Sentry (#169), like the same failure on the conversation path.
func TestSummarizeCancelledByShutdownIsNotReportedToSentry(t *testing.T) {
	counter := bindSentryEventCounter(t)

	p := newPipelineTestBot(t, config.BotConfig{AdminIDs: []int64{7}}, &dmCatalogSpy{})
	p.bot.extractor = &stubExtractor{article: extractor.Article{Title: "T", Text: strings.Repeat("word ", 50), URL: "https://example.com/a"}}
	p.bot.sanitizer = markdown.NewTgMarkdownV2Sanitizer()

	handlerCtx, cancelHandler := context.WithCancel(context.Background())
	defer cancelHandler()

	p.bot.llm = summarizerFunc(func(ctx context.Context) error {
		cancelHandler() // what bh.Stop() does to an in-flight handler
		<-ctx.Done()

		return errors.Join(llm.ErrLlmBackendRequestFailed, fmt.Errorf("post chat completions: %w", ctx.Err()))
	})

	if err := p.bh.BaseGroup().HandleUpdate(handlerCtx, p.bot.api, pipelinePrivateUpdate(7, "/summarize https://example.com/a")); err != nil {
		t.Fatalf("HandleUpdate: %v", err)
	}

	if got := counter.events.Load(); got != 0 {
		t.Fatalf("a cancelled /summarize produced %d Sentry event(s), want 0", got)
	}
}

// The command replies send with the handler context too: a send aborted by
// cancellation is not reported.
func TestCommandReplySendCancelledIsNotReportedToSentry(t *testing.T) {
	for _, command := range []string{"/help", "/start", "/stats", "/reset"} {
		t.Run(command, func(t *testing.T) {
			counter := bindSentryEventCounter(t)

			p := newPipelineTestBot(t, config.BotConfig{AdminIDs: []int64{7}}, &dmCatalogSpy{})
			p.bot.sanitizer = markdown.NewTgMarkdownV2Sanitizer()

			ctx, cancel := context.WithCancel(context.Background())
			cancel()

			if err := p.bh.BaseGroup().HandleUpdate(ctx, p.bot.api, pipelinePrivateUpdate(7, command)); err != nil {
				t.Fatalf("HandleUpdate: %v", err)
			}

			if got := counter.events.Load(); got != 0 {
				t.Fatalf("%s with a cancelled context produced %d Sentry event(s), want 0", command, got)
			}
		})
	}
}

// A genuine summarizer failure is still reported.
func TestSummarizeBackendFailureIsStillReportedToSentry(t *testing.T) {
	counter := bindSentryEventCounter(t)

	p := newPipelineTestBot(t, config.BotConfig{AdminIDs: []int64{7}}, &dmCatalogSpy{})
	p.bot.extractor = &stubExtractor{article: extractor.Article{Title: "T", Text: strings.Repeat("word ", 50), URL: "https://example.com/a"}}
	p.bot.sanitizer = markdown.NewTgMarkdownV2Sanitizer()
	p.bot.llm = summarizerFunc(func(context.Context) error {
		return errors.Join(llm.ErrLlmBackendRequestFailed, errors.New("backend returned 500"))
	})

	if err := p.bh.BaseGroup().HandleUpdate(context.Background(), p.bot.api, pipelinePrivateUpdate(7, "/summarize https://example.com/a")); err != nil {
		t.Fatalf("HandleUpdate: %v", err)
	}

	if got := counter.events.Load(); got == 0 {
		t.Fatal("a genuine summarizer failure must still be reported")
	}
}

// An unsolicited (silent) turn that runs past the processing deadline is a
// counted timeout, not a defect: like the explicit path, it must not reach
// Sentry, whether the run itself succeeded or failed without a context error
// of its own when the deadline lapsed (#169).
func TestSilentTurnTimeoutIsNotReportedToSentry(t *testing.T) {
	for name, workErr := range map[string]error{
		"work succeeded":           nil,
		"work failed without ctx":  errors.New("backend failure"),
		"work failed with ctx err": context.DeadlineExceeded,
	} {
		t.Run(name, func(t *testing.T) {
			counter := bindSentryEventCounter(t)

			p := newProactiveTestBot(t, nil)
			p.bot.cfg.ProcessingTimeout = time.Nanosecond // the deadline has passed when the work runs
			p.llm.err = workErr

			msg := *pipelineGroupUpdate(21, "hello").Message
			user := state.Message{MessageID: msg.MessageID, ChatID: msg.Chat.ID, FromID: 21, Text: msg.Text}

			effects := p.bot.runConversationTurn(context.Background(), msg, user, adminconfig.ResolvedChatConfig{}, turnOptions{
				trigger:     llmcontext.TriggerProactive,
				errorPolicy: silentErrors,
				delivery:    deliverAndRecord,
			})

			if effects.Failure == 0 {
				t.Fatalf("the turn must have failed with a timeout, got %+v", effects)
			}
			if got := counter.events.Load(); got != 0 {
				t.Fatalf("a silent-turn timeout produced %d Sentry event(s), want 0", got)
			}
		})
	}
}
