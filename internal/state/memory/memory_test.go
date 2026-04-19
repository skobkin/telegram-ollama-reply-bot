package memory

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"telegram-ollama-reply-bot/internal/state"
)

func TestConversationStoreKeepsBoundedHistoryPerScope(t *testing.T) {
	t.Parallel()

	stores := New(Config{
		HistoryMessagesPerStream: 2,
		HistoryStreamsMax:        8,
		HistoryMaxBytes:          1 << 20,
		ImageCacheMaxBytes:       1 << 20,
	}, slog.New(slog.NewJSONHandler(io.Discard, nil)))

	scope := state.ConversationScope{ChatID: 10}
	store := stores.Conversations()
	store.AppendMessage(scope, state.Message{Text: "one", FromID: 1})
	store.AppendMessage(scope, state.Message{Text: "two", FromID: 2})
	store.AppendMessage(scope, state.Message{Text: "three", FromID: 3})

	snapshot := store.Snapshot(scope)
	if len(snapshot.Messages) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(snapshot.Messages))
	}
	if snapshot.Messages[0].Text != "two" || snapshot.Messages[1].Text != "three" {
		t.Fatalf("unexpected retained messages: %#v", snapshot.Messages)
	}
}

func TestConversationStoreSeparatesTopics(t *testing.T) {
	t.Parallel()

	stores := New(Config{}, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	store := stores.Conversations()

	baseScope := state.ConversationScope{ChatID: 100, TopicID: 0}
	topicScope := state.ConversationScope{ChatID: 100, TopicID: 77}
	store.AppendMessage(baseScope, state.Message{Text: "chat-root"})
	store.AppendMessage(topicScope, state.Message{Text: "topic-only"})
	store.SetEarlierSummary(topicScope, "topic-summary", 1)

	baseSnapshot := store.Snapshot(baseScope)
	if len(baseSnapshot.Messages) != 1 || baseSnapshot.Messages[0].Text != "chat-root" {
		t.Fatalf("unexpected base scope snapshot: %#v", baseSnapshot.Messages)
	}

	topicSnapshot := store.Snapshot(topicScope)
	if len(topicSnapshot.Messages) != 1 || topicSnapshot.Messages[0].Text != "topic-only" {
		t.Fatalf("unexpected topic scope snapshot: %#v", topicSnapshot.Messages)
	}
	if topicSnapshot.EarlierSummary != "topic-summary" || topicSnapshot.SummarizedUntil != 1 {
		t.Fatalf("unexpected topic summary state: %#v", topicSnapshot)
	}
}

func TestConversationStoreResetChatDropsAllScopes(t *testing.T) {
	t.Parallel()

	stores := New(Config{}, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	store := stores.Conversations()

	store.AppendMessage(state.ConversationScope{ChatID: 1, TopicID: 0}, state.Message{Text: "root"})
	store.AppendMessage(state.ConversationScope{ChatID: 1, TopicID: 42}, state.Message{Text: "topic"})
	store.AppendMessage(state.ConversationScope{ChatID: 2, TopicID: 0}, state.Message{Text: "other chat"})

	store.ResetChat(1)

	if got := len(store.Snapshot(state.ConversationScope{ChatID: 1, TopicID: 0}).Messages); got != 0 {
		t.Fatalf("expected root scope reset, got %d messages", got)
	}
	if got := len(store.Snapshot(state.ConversationScope{ChatID: 1, TopicID: 42}).Messages); got != 0 {
		t.Fatalf("expected topic scope reset, got %d messages", got)
	}
	if got := len(store.Snapshot(state.ConversationScope{ChatID: 2, TopicID: 0}).Messages); got != 1 {
		t.Fatalf("expected other chat intact, got %d messages", got)
	}
}

func TestImageStoreExpiresEntries(t *testing.T) {
	t.Parallel()

	stores := New(Config{
		ImageCacheTTL:      15 * time.Millisecond,
		ImageCacheMaxBytes: 1 << 20,
	}, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	store := stores.Images()
	meta := &state.ImageMeta{FileUniqueID: "unique"}

	store.Set(meta, "description")
	if _, ok := store.Get(meta); !ok {
		t.Fatal("expected image description in cache")
	}

	time.Sleep(25 * time.Millisecond)

	if _, ok := store.Get(meta); ok {
		t.Fatal("expected image description to expire")
	}
}

func TestStatsStoreAccumulatesUsageAndCounters(t *testing.T) {
	t.Parallel()

	stores := New(Config{}, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	stats := stores.Stats()

	stats.PrivateRequest()
	stats.Mention()
	stats.AddUsage(10, 20, 30, 0.5)
	stats.LlmTimeout()

	snapshot := stats.Snapshot()
	if snapshot.PrivateRequests != 1 || snapshot.Mentions != 1 {
		t.Fatalf("unexpected counter snapshot: %#v", snapshot)
	}
	if snapshot.PromptTokens != 10 || snapshot.CompletionTokens != 20 || snapshot.TotalTokens != 30 {
		t.Fatalf("unexpected token snapshot: %#v", snapshot)
	}
	if snapshot.TotalCost != 0.5 || snapshot.LlmTimeouts != 1 {
		t.Fatalf("unexpected totals snapshot: %#v", snapshot)
	}
}
