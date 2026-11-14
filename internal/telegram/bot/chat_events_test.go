package bot

import (
	"context"
	"log/slog"
	"sync"
	"testing"

	tg "github.com/mymmrac/telego"
	th "github.com/mymmrac/telego/telegohandler"
	tu "github.com/mymmrac/telego/telegoutil"

	"telegram-ollama-reply-bot/internal/execution"
)

// chatEventsTestBot returns a rich-test bot with an event recorder wired as
// its dispatcher.
func chatEventsTestBot(t *testing.T, requests *[]capturedRequest, mu *sync.Mutex) (*Bot, *reactionEventRecorder) {
	t.Helper()

	b := newRichTestBot(t, requests, mu, false, false)
	rec := &reactionEventRecorder{}
	b.events = rec

	return b, rec
}

func requireMessageSentEvents(t *testing.T, rec *reactionEventRecorder, want int) []execution.MessageSent {
	t.Helper()

	events := rec.snapshot()
	if len(events) != want {
		t.Fatalf("expected %d published events, got %d: %T", want, len(events), events)
	}

	out := make([]execution.MessageSent, 0, want)
	for _, event := range events {
		sent, ok := event.(execution.MessageSent)
		if !ok {
			t.Fatalf("unexpected event type %T", event)
		}
		out = append(out, sent)
	}

	return out
}

func TestMessageSentPublishedOnSend(t *testing.T) {
	var requests []capturedRequest
	var mu sync.Mutex
	b, rec := chatEventsTestBot(t, &requests, &mu)

	sent, err := b.sendMessage(context.Background(), tu.Message(tu.ID(100), "plain text"))
	if err != nil {
		t.Fatalf("sendMessage: %v", err)
	}

	events := requireMessageSentEvents(t, rec, 1)
	if events[0].ChatID != 100 {
		t.Fatalf("expected chat id 100, got %d", events[0].ChatID)
	}
	if events[0].MessageID != sent.MessageID {
		t.Fatalf("expected message id %d, got %d", sent.MessageID, events[0].MessageID)
	}
	if events[0].RunID != "" {
		t.Fatalf("MessageSent is not run-bound: %+v", events[0].RunContext)
	}
	if events[0].At.IsZero() {
		t.Fatal("expected non-zero At")
	}
}

func TestMessageSentNotPublishedOnFailedSend(t *testing.T) {
	var requests []capturedRequest
	var mu sync.Mutex
	b := newRichTestBot(t, &requests, &mu, false, true)
	rec := &reactionEventRecorder{}
	b.events = rec

	if _, err := b.sendMessage(context.Background(), tu.Message(tu.ID(100), "text")); err == nil {
		t.Fatal("expected send failure")
	}
	requireMessageSentEvents(t, rec, 0)
}

func TestMessageSentNotPublishedWithoutDispatcher(t *testing.T) {
	var requests []capturedRequest
	var mu sync.Mutex
	b := newRichTestBot(t, &requests, &mu, false, false)
	b.events = nil

	if _, err := b.sendMessage(context.Background(), tu.Message(tu.ID(100), "text")); err != nil {
		t.Fatalf("sendMessage: %v", err)
	}
	message, text := richMessageTestInput()
	if _, err := b.sendRichMarkdownReply(context.Background(), message, text); err != nil {
		t.Fatalf("sendRichMarkdownReply: %v", err)
	}
}

func TestMessageSentPublishedOnRichReply(t *testing.T) {
	var requests []capturedRequest
	var mu sync.Mutex
	b, rec := chatEventsTestBot(t, &requests, &mu)

	message, text := richMessageTestInput()
	sent, err := b.sendRichMarkdownReply(context.Background(), message, text)
	if err != nil {
		t.Fatalf("sendRichMarkdownReply: %v", err)
	}

	events := requireMessageSentEvents(t, rec, 1)
	if events[0].ChatID != message.Chat.ID {
		t.Fatalf("expected chat id %d, got %d", message.Chat.ID, events[0].ChatID)
	}
	if events[0].MessageID != sent.MessageID {
		t.Fatalf("expected message id %d, got %d", sent.MessageID, events[0].MessageID)
	}
}

func TestIncomingMessageMiddlewarePublishes(t *testing.T) {
	textUpdate := tg.Update{UpdateID: 1, Message: &tg.Message{
		MessageID: 11,
		Chat:      tg.Chat{ID: 100500, Type: tg.ChatTypePrivate},
		From:      &tg.User{ID: 777},
		Text:      "hello bot",
	}}
	commandUpdate := tg.Update{UpdateID: 2, Message: &tg.Message{
		MessageID: 12,
		Chat:      tg.Chat{ID: 100500, Type: tg.ChatTypePrivate},
		From:      &tg.User{ID: 777},
		Text:      "/admin",
	}}
	photoUpdate := tg.Update{UpdateID: 3, Message: &tg.Message{
		MessageID: 13,
		Chat:      tg.Chat{ID: 100500, Type: tg.ChatTypePrivate},
		From:      &tg.User{ID: 777},
		Photo:     []tg.PhotoSize{{FileID: "f", Width: 1, Height: 1}},
	}}
	callbackUpdate := tg.Update{UpdateID: 4, CallbackQuery: &tg.CallbackQuery{
		ID:   "cb",
		From: tg.User{ID: 777},
		Data: "a:noop:1",
	}}

	updates := make(chan tg.Update, 4)
	for _, update := range []tg.Update{textUpdate, commandUpdate, photoUpdate, callbackUpdate} {
		updates <- update
	}
	close(updates)

	bh, err := th.NewBotHandler(nil, updates)
	if err != nil {
		t.Fatalf("NewBotHandler: %v", err)
	}

	rec := &reactionEventRecorder{}
	testBot := &Bot{
		ctx:    context.Background(),
		logger: slog.New(slog.NewTextHandler(testDiscardWriter{}, nil)),
		events: rec,
	}

	bh.Use(testBot.publishIncomingMessage)

	// Synchronize on the terminal handler: every update (including the
	// callback) flows through the middleware first, so four handler calls
	// mean all publications happened.
	var handled sync.WaitGroup
	handled.Add(4)
	bh.Handle(func(_ *th.Context, _ tg.Update) error {
		handled.Done()

		return nil
	})

	if err := bh.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	handled.Wait()

	events := requireMessageSentEvents(t, rec, 3)
	for i, wantMessageID := range []int{0, 0, 0} {
		if events[i].ChatID != 100500 {
			t.Fatalf("event %d: expected chat id 100500, got %d", i, events[i].ChatID)
		}
		if events[i].MessageID != wantMessageID {
			t.Fatalf("event %d: ingress carries no message id, got %d", i, events[i].MessageID)
		}
	}
}

// testDiscardWriter is an io.Writer discarding log output.
type testDiscardWriter struct{}

func (testDiscardWriter) Write(p []byte) (int, error) { return len(p), nil }
