package app

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	tg "github.com/mymmrac/telego"
	tu "github.com/mymmrac/telego/telegoutil"

	"telegram-ollama-reply-bot/internal/execution"
)

// eventRecorder captures published events behind a lock.
type eventRecorder struct {
	mu     sync.Mutex
	events []execution.Event
}

func (r *eventRecorder) Publish(event execution.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, event)
}

func (r *eventRecorder) snapshot() []execution.Event {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]execution.Event(nil), r.events...)
}

// eventsTestAPI is a fake Telegram Bot API server: every call succeeds with a
// message result unless the method is marked failing.
type eventsTestAPI struct {
	server *httptest.Server

	mu     sync.Mutex
	fail   map[string]bool
	sent   []string
	nextID int
}

func newEventsTestAPI(t *testing.T, failing ...string) *eventsTestAPI {
	t.Helper()

	fake := &eventsTestAPI{
		fail:   make(map[string]bool),
		nextID: 500,
	}
	for _, method := range failing {
		fake.fail[method] = true
	}

	fake.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := strings.TrimPrefix(r.URL.Path, "/bot1:"+strings.Repeat("A", 35)+"/")

		fake.mu.Lock()
		fake.sent = append(fake.sent, method)
		shouldFail := fake.fail[method]
		fake.mu.Unlock()

		if shouldFail {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "description": "forced failure"})

			return
		}

		w.Header().Set("Content-Type", "application/json")
		// setMessageReaction returns True, not a message object: telego
		// decodes the result into bool.
		if method == "setMessageReaction" {
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": true})

			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": true,
			"result": map[string]any{
				"message_id": fake.nextID,
				"chat":       map[string]any{"id": 100, "type": "private"},
			},
		})
	}))
	t.Cleanup(fake.server.Close)

	return fake
}

func newEventsTestBot(t *testing.T, fake *eventsTestAPI, events execution.Dispatcher) *eventPublishingBot {
	t.Helper()

	api, err := tg.NewBot("1:"+strings.Repeat("A", 35), tg.WithAPIServer(fake.server.URL), tg.WithDiscardLogger())
	if err != nil {
		t.Fatalf("NewBot: %v", err)
	}

	return &eventPublishingBot{api: api, events: events}
}

func requireMessageSent(t *testing.T, rec *eventRecorder, want int) []execution.MessageSent {
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

// TestEventPublishingBotPublishesOnSuccess proves every wrapped send path
// publishes MessageSent with the target chat and the delivered message ID.
func TestEventPublishingBotPublishesOnSuccess(t *testing.T) {
	rec := &eventRecorder{}
	bot := newEventsTestBot(t, newEventsTestAPI(t), rec)
	ctx := context.Background()

	message, err := bot.SendMessage(ctx, tu.Message(tu.ID(100), "reminder text"))
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	poll, err := bot.SendPoll(ctx, &tg.SendPollParams{
		ChatID:   tu.ID(100),
		Question: "pick one",
		Options:  []tg.InputPollOption{{Text: "a"}, {Text: "b"}},
	})
	if err != nil {
		t.Fatalf("SendPoll: %v", err)
	}

	dice, err := bot.SendDice(ctx, &tg.SendDiceParams{ChatID: tu.ID(100)})
	if err != nil {
		t.Fatalf("SendDice: %v", err)
	}

	photo, err := bot.SendPhoto(ctx, tu.Photo(tu.ID(100), tu.FileFromBytes([]byte("png"), "chart.png")))
	if err != nil {
		t.Fatalf("SendPhoto: %v", err)
	}

	sticker, err := bot.SendSticker(ctx, tu.Sticker(tu.ID(100), tu.FileFromID("sticker-file-id")))
	if err != nil {
		t.Fatalf("SendSticker: %v", err)
	}

	events := requireMessageSent(t, rec, 5)
	for i, want := range []struct {
		messageID int
	}{
		{message.MessageID},
		{poll.MessageID},
		{dice.MessageID},
		{photo.MessageID},
		{sticker.MessageID},
	} {
		if events[i].ChatID != 100 {
			t.Fatalf("event %d: expected chat id 100, got %d", i, events[i].ChatID)
		}
		if events[i].MessageID != want.messageID {
			t.Fatalf("event %d: expected message id %d, got %d", i, want.messageID, events[i].MessageID)
		}
		if events[i].At.IsZero() {
			t.Fatalf("event %d: expected non-zero At", i)
		}
	}
}

// TestEventPublishingBotSilentOnFailure proves a failed send publishes
// nothing: the event must describe delivered messages only.
func TestEventPublishingBotSilentOnFailure(t *testing.T) {
	rec := &eventRecorder{}
	fake := newEventsTestAPI(t, "sendMessage", "sendPoll", "sendDice", "sendPhoto", "sendSticker")
	bot := newEventsTestBot(t, fake, rec)
	ctx := context.Background()

	if _, err := bot.SendMessage(ctx, tu.Message(tu.ID(100), "text")); err == nil {
		t.Fatal("expected SendMessage failure")
	}
	if _, err := bot.SendPoll(ctx, &tg.SendPollParams{ChatID: tu.ID(100), Question: "q", Options: []tg.InputPollOption{{Text: "a"}}}); err == nil {
		t.Fatal("expected SendPoll failure")
	}
	if _, err := bot.SendDice(ctx, &tg.SendDiceParams{ChatID: tu.ID(100)}); err == nil {
		t.Fatal("expected SendDice failure")
	}
	if _, err := bot.SendPhoto(ctx, tu.Photo(tu.ID(100), tu.FileFromBytes([]byte("png"), "chart.png"))); err == nil {
		t.Fatal("expected SendPhoto failure")
	}
	if _, err := bot.SendSticker(ctx, tu.Sticker(tu.ID(100), tu.FileFromID("sticker-file-id"))); err == nil {
		t.Fatal("expected SendSticker failure")
	}

	requireMessageSent(t, rec, 0)
}

// TestEventPublishingBotSendStickerPublishesOnlyConfirmedSends proves a
// "successful" response that carries no message id is returned untouched for
// the tool to classify and publishes no MessageSent: it confirms nothing.
func TestEventPublishingBotSendStickerPublishesOnlyConfirmedSends(t *testing.T) {
	rec := &eventRecorder{}
	fake := newEventsTestAPI(t)
	fake.nextID = 0
	bot := newEventsTestBot(t, fake, rec)

	message, err := bot.SendSticker(context.Background(), tu.Sticker(tu.ID(100), tu.FileFromID("sticker-file-id")))
	if err != nil {
		t.Fatalf("SendSticker: %v", err)
	}
	if message == nil || message.MessageID != 0 {
		t.Fatalf("the unconfirmed response must be returned untouched, got %+v", message)
	}
	requireMessageSent(t, rec, 0)
}

// TestEventPublishingBotSetMessageReactionPublishesNothing proves the
// reaction path forwards to the API but publishes no MessageSent: a reaction
// is not a message, and the run-correlated BotReactionSet is published by the
// tool runtime instead.
func TestEventPublishingBotSetMessageReactionPublishesNothing(t *testing.T) {
	rec := &eventRecorder{}
	fake := newEventsTestAPI(t)
	bot := newEventsTestBot(t, fake, rec)
	ctx := context.Background()

	params := &tg.SetMessageReactionParams{
		ChatID:    tu.ID(100),
		MessageID: 42,
		Reaction:  []tg.ReactionType{&tg.ReactionTypeEmoji{Type: tg.ReactionEmoji, Emoji: "👍"}},
	}
	if err := bot.SetMessageReaction(ctx, params); err != nil {
		t.Fatalf("SetMessageReaction: %v", err)
	}

	fake.mu.Lock()
	sent := append([]string(nil), fake.sent...)
	fake.mu.Unlock()
	if len(sent) != 1 || sent[0] != "setMessageReaction" {
		t.Fatalf("expected exactly one setMessageReaction API call, got %v", sent)
	}

	requireMessageSent(t, rec, 0)
}

// TestEventPublishingBotSetMessageReactionForwardsFailure proves API errors
// pass through unchanged and publish nothing.
func TestEventPublishingBotSetMessageReactionForwardsFailure(t *testing.T) {
	rec := &eventRecorder{}
	fake := newEventsTestAPI(t, "setMessageReaction")
	bot := newEventsTestBot(t, fake, rec)

	if err := bot.SetMessageReaction(context.Background(), &tg.SetMessageReactionParams{
		ChatID:    tu.ID(100),
		MessageID: 42,
	}); err == nil {
		t.Fatal("expected SetMessageReaction failure")
	}

	requireMessageSent(t, rec, 0)
}

// TestEventPublishingBotNilDispatcher proves the wrapper is usable without a
// dispatcher: sends succeed, nothing panics.
func TestEventPublishingBotNilDispatcher(t *testing.T) {
	bot := newEventsTestBot(t, newEventsTestAPI(t), nil)

	message, err := bot.SendMessage(context.Background(), tu.Message(tu.ID(100), "text"))
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if message.MessageID == 0 {
		t.Fatal("expected a delivered message")
	}
}

// TestEventPublishingBotWithRealDispatcher runs one send through a live
// dispatcher to prove the published event reaches subscribers intact.
func TestEventPublishingBotWithRealDispatcher(t *testing.T) {
	dispatcher := execution.NewDispatcher(execution.DispatcherConfig{QueueSize: 8}, slog.New(slog.DiscardHandler))
	dispatcher.Start(context.Background())
	t.Cleanup(dispatcher.Stop)

	rec := &eventRecorder{}
	dispatcher.Subscribe(execution.HandlerFunc(func(_ context.Context, event execution.Event) {
		rec.Publish(event)
	}))

	bot := newEventsTestBot(t, newEventsTestAPI(t), dispatcher)

	if _, err := bot.SendMessage(context.Background(), tu.Message(tu.ID(100), "text")); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	// Delivery is asynchronous; wait for the subscriber to see the event.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(rec.snapshot()) == 0 {
		time.Sleep(time.Millisecond)
	}

	requireMessageSent(t, rec, 1)
}
