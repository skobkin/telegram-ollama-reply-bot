package bot

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"telegram-ollama-reply-bot/internal/adminconfig"
	"telegram-ollama-reply-bot/internal/execution"
	"telegram-ollama-reply-bot/internal/state"

	tg "github.com/mymmrac/telego"
)

// reactionEventRecorder is a minimal execution.Dispatcher that records
// published events for assertions, mirroring the tooluse recording
// dispatcher pattern.
type reactionEventRecorder struct {
	mu     sync.Mutex
	events []execution.Event
}

func (r *reactionEventRecorder) Publish(event execution.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, event)
}

func (r *reactionEventRecorder) snapshot() []execution.Event {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]execution.Event(nil), r.events...)
}

func (r *reactionEventRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()

	return len(r.events)
}

// requireReactionEvents asserts the recorder holds exactly want events and
// returns them typed as ReactionChanged.
func requireReactionEvents(t *testing.T, rec *reactionEventRecorder, want int) []execution.ReactionChanged {
	t.Helper()

	events := rec.snapshot()
	if len(events) != want {
		t.Fatalf("expected %d published events, got %d", want, len(events))
	}

	out := make([]execution.ReactionChanged, 0, len(events))
	for _, event := range events {
		reaction, ok := event.(execution.ReactionChanged)
		if !ok {
			t.Fatalf("unexpected event type %T", event)
		}
		out = append(out, reaction)
	}

	return out
}

// TestReactionChangedEventPopulated proves authorized tracked additions,
// replacements, and removals publish correctly populated events: exact
// envelope UpdateID, Telegram timestamp, actor attribution, and the full
// tracked set with an empty set for removals.
func TestReactionChangedEventPopulated(t *testing.T) {
	store := newMemoryConversationStore()
	rec := &reactionEventRecorder{}
	p := newReactionTestBot(t, &dmCatalogSpy{}, store)
	p.bot.events = rec

	scope := state.ConversationScope{ChatID: 100500}
	p.store.AppendMessage(scope, state.Message{IsMe: true, MessageID: 777, Text: "answer"})

	actor := &tg.User{ID: 42, FirstName: "Alice", LastName: "L", Username: "alice"}
	cases := []struct {
		name       string
		updateID   int
		newList    []tg.ReactionType
		oldTracked bool
		want       []string
	}{
		{name: "addition", updateID: 101, newList: emojiReaction("👍"), want: []string{"👍"}},
		{name: "replacement", updateID: 102, newList: emojiReaction("🔥"), oldTracked: true, want: []string{"🔥"}},
		{name: "removal", updateID: 103, newList: nil, oldTracked: true, want: []string{}},
	}

	// Updates carry strictly increasing envelope IDs, so the cumulative
	// published count grows with each case.
	for _, testCase := range cases {
		update := reactionUpdateWithID(testCase.updateID, 100500, 777, actor, testCase.newList)
		if testCase.oldTracked {
			update.MessageReaction.OldReaction = emojiReaction("👍")
		}
		p.deliver(t, update)

		events := requireReactionEvents(t, rec, testCase.updateID-100)
		event := events[len(events)-1]
		if event.ChatID != 100500 || event.TargetMessageID != 777 {
			t.Fatalf("%s: event must identify chat and target message, got %+v", testCase.name, event)
		}
		if event.RunID != "" || event.TopicID != 0 {
			t.Fatalf("%s: reaction events are not run-bound, got %+v", testCase.name, event.RunContext)
		}
		if event.UserID != 42 || event.Name != "Alice L" || event.Username != "alice" {
			t.Fatalf("%s: event must carry actor attribution, got %+v", testCase.name, event)
		}
		if event.UpdateID != int64(testCase.updateID) {
			t.Fatalf("%s: event must retain the envelope UpdateID %d, got %d", testCase.name, testCase.updateID, event.UpdateID)
		}
		if want := time.Unix(1700000000, 0).UTC(); !event.At.Equal(want) {
			t.Fatalf("%s: event must carry the Telegram timestamp %v, got %v", testCase.name, want, event.At)
		}
		if len(event.Reactions) != len(testCase.want) {
			t.Fatalf("%s: expected reactions %v, got %v", testCase.name, testCase.want, event.Reactions)
		}
		for i, emoji := range testCase.want {
			if event.Reactions[i] != emoji {
				t.Fatalf("%s: expected reactions %v, got %v", testCase.name, testCase.want, event.Reactions)
			}
		}
	}
}

// TestReactionChangedSwitchAwayFromTrackedPublishesRemoval proves a change
// away from a tracked emoji retains removal semantics in the event: the
// update stays eligible and publishes an empty tracked set.
func TestReactionChangedSwitchAwayFromTrackedPublishesRemoval(t *testing.T) {
	rec := &reactionEventRecorder{}
	p := newReactionTestBot(t, &dmCatalogSpy{}, newMemoryConversationStore())
	p.bot.events = rec

	switchAway := reactionUpdateWithID(202, 100500, 777, &tg.User{ID: 42, FirstName: "Alice"}, emojiReaction("🍌"))
	switchAway.MessageReaction.OldReaction = emojiReaction("👍")
	p.deliver(t, switchAway)

	events := requireReactionEvents(t, rec, 1)
	if len(events[0].Reactions) != 0 {
		t.Fatalf("switching to an untracked emoji must publish an empty tracked set, got %v", events[0].Reactions)
	}
	if events[0].UpdateID != 202 {
		t.Fatalf("event must retain the envelope UpdateID, got %d", events[0].UpdateID)
	}
}

// TestReactionChangedEventMapsGroupTarget proves group and forum-topic
// reactions map chat-wide to the right bot message: the event identifies
// chat and target message without inventing a topic.
func TestReactionChangedEventMapsGroupTarget(t *testing.T) {
	rec := &reactionEventRecorder{}
	p := newReactionTestBot(t, &dmCatalogSpy{}, newMemoryConversationStore())
	p.bot.events = rec

	scope := state.ConversationScope{ChatID: -100999, TopicID: 5}
	p.store.AppendMessage(scope, state.Message{IsMe: true, MessageID: 777, Text: "answer"})

	update := reactionUpdateWithID(101, -100999, 777, &tg.User{ID: 42, FirstName: "Alice"}, emojiReaction("👍"))
	update.MessageReaction.Chat = tg.Chat{ID: -100999, Type: tg.ChatTypeSupergroup, Title: "Test Group"}
	p.deliver(t, update)

	events := requireReactionEvents(t, rec, 1)
	if events[0].ChatID != -100999 || events[0].TargetMessageID != 777 {
		t.Fatalf("event must map the group target chat-wide, got %+v", events[0])
	}
	if events[0].TopicID != 0 || events[0].RunID != "" {
		t.Fatalf("reaction updates carry no topic and no run, got %+v", events[0].RunContext)
	}
}

// TestReactionChangedNotPublishedForIneligible proves rejected or
// irrelevant updates produce no events: anonymous actors, untracked-only
// changes, custom and paid reaction types, access denials, and the
// DM-admins-only restriction.
func TestReactionChangedNotPublishedForIneligible(t *testing.T) {
	cases := []struct {
		name         string
		store        adminconfig.Store
		dmAdminsOnly bool
		user         *tg.User
		newList      []tg.ReactionType
	}{
		{name: "anonymous", store: &dmCatalogSpy{}, user: nil, newList: emojiReaction("👍")},
		{name: "untracked only", store: &dmCatalogSpy{}, user: &tg.User{ID: 42, FirstName: "Alice"}, newList: emojiReaction("🍌")},
		{name: "custom emoji", store: &dmCatalogSpy{}, user: &tg.User{ID: 42, FirstName: "Alice"}, newList: []tg.ReactionType{&tg.ReactionTypeCustomEmoji{Type: tg.ReactionCustomEmoji, CustomEmojiID: "42"}}},
		{name: "paid", store: &dmCatalogSpy{}, user: &tg.User{ID: 42, FirstName: "Alice"}, newList: []tg.ReactionType{&tg.ReactionTypePaid{Type: tg.ReactionPaid}}},
		{name: "non-whitelisted chat", store: &adminStoreStub{}, user: &tg.User{ID: 42, FirstName: "Alice"}, newList: emojiReaction("👍")},
		{name: "non-admin DM with DMAdminsOnly", store: &dmCatalogSpy{}, dmAdminsOnly: true, user: &tg.User{ID: 42, FirstName: "Alice"}, newList: emojiReaction("👍")},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			spy := &dmHistorySpy{}
			rec := &reactionEventRecorder{}
			p := newReactionTestBot(t, testCase.store, spy)
			p.bot.events = rec
			p.bot.cfg.DMAdminsOnly = testCase.dmAdminsOnly

			p.deliver(t, reactionUpdate(100500, 777, testCase.user, testCase.newList))

			if got := rec.count(); got != 0 {
				t.Fatalf("ineligible update must not publish an event, got %d", got)
			}
			if got := spy.reactionCallCount(); got != 0 {
				t.Fatalf("ineligible update must not touch the store, got %d calls", got)
			}
		})
	}
}

// TestReactionChangedPublishedOnceAcrossPendingReplay proves the
// publish-once guarantee across the pending-reaction path: a held update
// publishes at ingress, and replaying it on history insertion must not
// publish again — while history still ends up correct.
func TestReactionChangedPublishedOnceAcrossPendingReplay(t *testing.T) {
	store := newMemoryConversationStore()
	rec := &reactionEventRecorder{}
	p := newReactionTestBot(t, &dmCatalogSpy{}, store)
	p.bot.events = rec

	// The reply is visible on Telegram before its send response lands:
	// the reaction is held, but eligibility (and the event) happens now.
	p.deliver(t, reactionUpdateWithID(101, 100500, 777, &tg.User{ID: 42, FirstName: "Alice"}, emojiReaction("👍")))
	events := requireReactionEvents(t, rec, 1)
	if events[0].UpdateID != 101 {
		t.Fatalf("held update must publish at ingress with its UpdateID, got %d", events[0].UpdateID)
	}

	// History insertion replays the held update: no second event.
	replyTo := tg.Message{MessageID: 55, Chat: tg.Chat{ID: 100500, Type: tg.ChatTypePrivate}}
	p.bot.saveBotReplyToHistory(context.Background(), replyTo, "answer", 777)

	requireReactionEvents(t, rec, 1)

	snapshot := store.Snapshot(state.ConversationScope{ChatID: 100500})
	if len(snapshot.Messages) != 1 || len(snapshot.Messages[0].Reactions) != 1 {
		t.Fatalf("replay must still apply the held reaction to history: %+v", snapshot.Messages)
	}
}

// TestReactionProcessingIndependentOfEventDelivery proves event-delivery
// failures cannot affect authoritative reaction state: with no dispatcher,
// a stopped dispatcher, a panicking subscriber, or an overflowing
// subscriber, the tracked reaction still lands in history synchronously.
func TestReactionProcessingIndependentOfEventDelivery(t *testing.T) {
	t.Run("no dispatcher", func(t *testing.T) {
		p := newReactionTestBot(t, &dmCatalogSpy{}, newMemoryConversationStore())

		scope := state.ConversationScope{ChatID: 100500}
		p.store.AppendMessage(scope, state.Message{IsMe: true, MessageID: 777, Text: "answer"})
		p.deliver(t, reactionUpdate(100500, 777, &tg.User{ID: 42, FirstName: "Alice"}, emojiReaction("👍")))

		snapshot := p.store.Snapshot(scope)
		if len(snapshot.Messages[0].Reactions) != 1 {
			t.Fatalf("reaction must be recorded without any dispatcher: %+v", snapshot.Messages[0].Reactions)
		}
	})

	t.Run("stopped dispatcher", func(t *testing.T) {
		p := newReactionTestBot(t, &dmCatalogSpy{}, newMemoryConversationStore())

		var delivered atomic.Int32
		d := execution.NewDispatcher(execution.DispatcherConfig{QueueSize: 16}, reactionTestLogger())
		d.Start(context.Background())
		d.Subscribe(execution.HandlerFunc(func(_ context.Context, _ execution.Event) {
			delivered.Add(1)
		}))
		d.Stop()
		p.bot.events = d

		scope := state.ConversationScope{ChatID: 100500}
		p.store.AppendMessage(scope, state.Message{IsMe: true, MessageID: 777, Text: "answer"})
		p.deliver(t, reactionUpdate(100500, 777, &tg.User{ID: 42, FirstName: "Alice"}, emojiReaction("👍")))

		snapshot := p.store.Snapshot(scope)
		if len(snapshot.Messages[0].Reactions) != 1 {
			t.Fatalf("reaction must be recorded with a stopped dispatcher: %+v", snapshot.Messages[0].Reactions)
		}
		if got := delivered.Load(); got != 0 {
			t.Fatalf("publish after Stop must be a no-op, got %d events", got)
		}
	})

	t.Run("panicking subscriber", func(t *testing.T) {
		p := newReactionTestBot(t, &dmCatalogSpy{}, newMemoryConversationStore())

		d := execution.NewDispatcher(execution.DispatcherConfig{QueueSize: 16}, reactionTestLogger())
		d.Start(context.Background())
		t.Cleanup(d.Stop)
		d.Subscribe(execution.HandlerFunc(func(_ context.Context, _ execution.Event) {
			panic("boom")
		}))
		p.bot.events = d

		scope := state.ConversationScope{ChatID: 100500}
		p.store.AppendMessage(scope, state.Message{IsMe: true, MessageID: 777, Text: "answer"})
		p.deliver(t, reactionUpdate(100500, 777, &tg.User{ID: 42, FirstName: "Alice"}, emojiReaction("👍")))

		snapshot := p.store.Snapshot(scope)
		if len(snapshot.Messages[0].Reactions) != 1 {
			t.Fatalf("subscriber panic must not lose the reaction: %+v", snapshot.Messages[0].Reactions)
		}
	})

	t.Run("overflowing dispatcher", func(t *testing.T) {
		p := newReactionTestBot(t, &dmCatalogSpy{}, newMemoryConversationStore())

		d := execution.NewDispatcher(execution.DispatcherConfig{QueueSize: 1}, reactionTestLogger())
		d.Start(context.Background())
		t.Cleanup(d.Stop)
		release := make(chan struct{})
		blocked := make(chan struct{}, 1)
		d.Subscribe(execution.HandlerFunc(func(_ context.Context, _ execution.Event) {
			blocked <- struct{}{}
			<-release
		}))
		p.bot.events = d

		// Park the delivery goroutine inside its handler and fill the queue.
		d.Publish(execution.RunStarted{RunContext: execution.RunContext{RunID: "x"}, At: time.Now()})
		waitForCondition(t, 2*time.Second, "subscriber delivering", func() bool {
			select {
			case <-blocked:
				return true
			default:
				return false
			}
		})
		d.Publish(execution.RunStarted{RunContext: execution.RunContext{RunID: "y"}, At: time.Now()})

		scope := state.ConversationScope{ChatID: 100500}
		p.store.AppendMessage(scope, state.Message{IsMe: true, MessageID: 777, Text: "answer"})

		// The reaction update must be applied synchronously; publication
		// onto the backed-up dispatcher must not block the ingress.
		p.deliver(t, reactionUpdateWithID(101, 100500, 777, &tg.User{ID: 42, FirstName: "Alice"}, emojiReaction("👍")))
		close(release)

		snapshot := p.store.Snapshot(scope)
		if len(snapshot.Messages[0].Reactions) != 1 {
			t.Fatalf("overflow must not lose the authoritative update: %+v", snapshot.Messages[0].Reactions)
		}
	})
}

// TestReactionConcurrentDeliveriesPublishSafely proves concurrent handler
// executions stay correct with a publisher wired: every eligible update
// lands in history and publishes exactly one event, in any delivery order
// — the store's UpdateID comparison remains the ordering authority.
func TestReactionConcurrentDeliveriesPublishSafely(t *testing.T) {
	store := newMemoryConversationStore()
	rec := &reactionEventRecorder{}
	p := newReactionTestBot(t, &dmCatalogSpy{}, store)
	p.bot.events = rec

	scope := state.ConversationScope{ChatID: 100500}
	p.store.AppendMessage(scope, state.Message{IsMe: true, MessageID: 777, Text: "answer"})

	const users = 8
	var wg sync.WaitGroup
	for u := range users {
		wg.Add(1)
		go func() {
			defer wg.Done()
			update := reactionUpdateWithID(200+u, 100500, 777, &tg.User{ID: int64(40 + u), FirstName: "U"}, emojiReaction("👍"))
			if err := p.bh.BaseGroup().HandleUpdate(context.Background(), p.bot.api, update); err != nil {
				t.Errorf("HandleUpdate: %v", err)
			}
		}()
	}
	wg.Wait()

	reactions := store.Snapshot(scope).Messages[0].Reactions
	if len(reactions) != users {
		t.Fatalf("every concurrent reaction must be recorded, got %d: %+v", len(reactions), reactions)
	}
	seen := make(map[int64]struct{}, users)
	for _, reaction := range reactions {
		seen[reaction.UserID] = struct{}{}
	}
	if len(seen) != users {
		t.Fatalf("every user must keep their own reaction, got %+v", seen)
	}

	events := requireReactionEvents(t, rec, users)
	delivered := make(map[int64]struct{}, users)
	for _, event := range events {
		delivered[event.UserID] = struct{}{}
	}
	if len(delivered) != users {
		t.Fatalf("every eligible update must publish exactly one event, got users %+v", delivered)
	}
}

// TestReactionChangedNoTelegramSideEffects proves the refactoring generates
// no new Telegram messages, replies, or reactions: reaction processing and
// event publication are purely local.
func TestReactionChangedNoTelegramSideEffects(t *testing.T) {
	rec := &reactionEventRecorder{}
	p := newReactionTestBot(t, &dmCatalogSpy{}, newMemoryConversationStore())
	p.bot.events = rec

	scope := state.ConversationScope{ChatID: 100500}
	p.store.AppendMessage(scope, state.Message{IsMe: true, MessageID: 777, Text: "answer"})

	p.deliver(t, reactionUpdateWithID(101, 100500, 777, &tg.User{ID: 42, FirstName: "Alice"}, emojiReaction("👍")))

	if got := rec.count(); got != 1 {
		t.Fatalf("expected exactly one published event, got %d", got)
	}
	for _, method := range []string{"sendMessage", "sendRichMessage", "sendRichMessageDraft", "editMessageText", "sendChatAction", "setMessageReaction"} {
		if got := p.api.callCount(method); got != 0 {
			t.Fatalf("reaction processing must not call %s, got %d calls", method, got)
		}
	}
}
