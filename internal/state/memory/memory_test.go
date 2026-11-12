package memory

import (
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"telegram-ollama-reply-bot/internal/state"
)

func TestConversationStoreKeepsFullRawHistoryUntilGlobalLimits(t *testing.T) {
	t.Parallel()

	stores := New(Config{
		HistoryStreamsMax:  8,
		HistoryMaxBytes:    1 << 20,
		ImageCacheMaxBytes: 1 << 20,
	}, slog.New(slog.NewJSONHandler(io.Discard, nil)))

	scope := state.ConversationScope{ChatID: 10}
	store := stores.Conversations()
	store.AppendMessage(scope, state.Message{Text: "one", FromID: 1})
	store.AppendMessage(scope, state.Message{Text: "two", FromID: 2})
	store.AppendMessage(scope, state.Message{Text: "three", FromID: 3})

	snapshot := store.Snapshot(scope)
	if len(snapshot.Messages) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(snapshot.Messages))
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
	if topicSnapshot.EarlierSummary != "topic-summary" || topicSnapshot.SummaryMessageCount != 1 {
		t.Fatalf("unexpected topic summary state: %#v", topicSnapshot)
	}
}

func TestConversationStoreTrimsOversizedSingleScopeAndClearsSummary(t *testing.T) {
	t.Parallel()

	stores := New(Config{
		HistoryMaxBytes:    700,
		HistoryStreamsMax:  8,
		ImageCacheMaxBytes: 1 << 20,
	}, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	store := stores.Conversations()
	scope := state.ConversationScope{ChatID: 10}

	store.AppendMessage(scope, state.Message{Text: "first message that is long enough", FromID: 1})
	store.AppendMessage(scope, state.Message{Text: "second message that is also long enough", FromID: 2})
	store.SetEarlierSummary(scope, "cached summary", 1)
	store.AppendMessage(scope, state.Message{Text: "third message that forces trimming by bytes", FromID: 3})

	snapshot := store.Snapshot(scope)
	if len(snapshot.Messages) == 0 {
		t.Fatal("expected some raw history to remain after trimming")
	}
	if snapshot.Messages[0].Text == "first message that is long enough" {
		t.Fatalf("expected oldest message to be trimmed, got %#v", snapshot.Messages)
	}
	if snapshot.EarlierSummary != "" || snapshot.SummaryMessageCount != 0 {
		t.Fatalf("expected summary cache reset after head trim, got %#v", snapshot)
	}
}

func TestConversationStoreSearchIndexCountsTowardApproxBytes(t *testing.T) {
	t.Parallel()

	stores := New(Config{
		HistoryStreamsMax:  8,
		HistoryMaxBytes:    1 << 20,
		ImageCacheMaxBytes: 1 << 20,
	}, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	scope := state.ConversationScope{ChatID: 10}
	store := stores.Conversations()
	msg := state.Message{Text: "Appointments are listed here", FromID: 1}
	store.AppendMessage(scope, msg)

	snapshot := store.Snapshot(scope)
	if snapshot.ApproxBytes <= messageApproxBytes(msg) {
		t.Fatalf("expected search index overhead in approx bytes, got %d", snapshot.ApproxBytes)
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

func TestConversationStoreSetMessageReactionsAcrossTopics(t *testing.T) {
	t.Parallel()

	stores := New(Config{}, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	store := stores.Conversations()

	// The bot reply lives in a topic stream; reaction updates carry no topic,
	// so the lookup must find it across the chat's streams.
	baseScope := state.ConversationScope{ChatID: 100, TopicID: 0}
	topicScope := state.ConversationScope{ChatID: 100, TopicID: 77}
	store.AppendMessage(baseScope, state.Message{Text: "user question", FromID: 1, MessageID: 10})
	store.AppendMessage(topicScope, state.Message{IsMe: true, MessageID: 11, Text: "bot answer"})

	if !store.SetMessageReactions(100, 11, 42, []state.Reaction{{UserID: 42, Name: "Alice", Emoji: "👍"}}) {
		t.Fatalf("expected the bot message to be found")
	}

	snapshot := store.Snapshot(topicScope)
	if len(snapshot.Messages[0].Reactions) != 1 || snapshot.Messages[0].Reactions[0].Emoji != "👍" {
		t.Fatalf("unexpected reactions on bot reply: %#v", snapshot.Messages[0].Reactions)
	}
	if snapshot.Messages[0].Reactions[0].UpdatedAt.IsZero() {
		t.Fatalf("expected reaction timestamp to be set")
	}

	// User messages are never reaction targets.
	if store.SetMessageReactions(100, 10, 42, []state.Reaction{{UserID: 42, Emoji: "👎"}}) {
		t.Fatalf("user message must not be a reaction target")
	}
	if len(store.Snapshot(baseScope).Messages[0].Reactions) != 0 {
		t.Fatalf("user message must not gain reactions")
	}
}

func TestConversationStoreSetMessageReactionsPerUserReplaceAndRemove(t *testing.T) {
	t.Parallel()

	stores := New(Config{}, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	store := stores.Conversations()

	scope := state.ConversationScope{ChatID: 200}
	store.AppendMessage(scope, state.Message{IsMe: true, MessageID: 20, Text: "answer"})

	store.SetMessageReactions(200, 20, 1, []state.Reaction{{UserID: 1, Emoji: "👍"}})
	store.SetMessageReactions(200, 20, 1, []state.Reaction{{UserID: 1, Emoji: "👎"}})
	store.SetMessageReactions(200, 20, 2, []state.Reaction{{UserID: 2, Emoji: "🔥"}})

	reactions := store.Snapshot(scope).Messages[0].Reactions
	if len(reactions) != 2 || reactions[0].UserID != 1 || reactions[0].Emoji != "👎" || reactions[1].UserID != 2 {
		t.Fatalf("per-user replace failed: %#v", reactions)
	}

	// Empty slice removes only that user's reactions.
	if !store.SetMessageReactions(200, 20, 1, nil) {
		t.Fatalf("expected removal to report the message as found")
	}
	reactions = store.Snapshot(scope).Messages[0].Reactions
	if len(reactions) != 1 || reactions[0].UserID != 2 {
		t.Fatalf("removal must keep other users: %#v", reactions)
	}
}

func TestConversationStoreSetMessageReactionsUnmapped(t *testing.T) {
	t.Parallel()

	stores := New(Config{}, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	store := stores.Conversations()

	scope := state.ConversationScope{ChatID: 300}
	store.AppendMessage(scope, state.Message{IsMe: true, MessageID: 30, Text: "answer with no message id"})

	if store.SetMessageReactions(300, 999, 1, []state.Reaction{{UserID: 1, Emoji: "👍"}}) {
		t.Fatalf("unknown message ID must not be found")
	}
	if store.SetMessageReactions(400, 30, 1, []state.Reaction{{UserID: 1, Emoji: "👍"}}) {
		t.Fatalf("other chat must not be searched")
	}
	if store.SetMessageReactions(300, 0, 1, nil) || store.SetMessageReactions(300, -5, 1, nil) {
		t.Fatalf("non-positive message IDs must be rejected")
	}
	if len(store.Snapshot(scope).Messages[0].Reactions) != 0 {
		t.Fatalf("unmapped targets must not modify history")
	}
}

func TestConversationStoreSetMessageReactionsGrowsApproxBytes(t *testing.T) {
	t.Parallel()

	stores := New(Config{}, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	store := stores.Conversations()

	scope := state.ConversationScope{ChatID: 400}
	store.AppendMessage(scope, state.Message{IsMe: true, MessageID: 40, Text: "answer"})

	before := store.Snapshot(scope).ApproxBytes
	store.SetMessageReactions(400, 40, 1, []state.Reaction{{UserID: 1, Name: "Alice", Username: "alice", Emoji: "👍"}})
	after := store.Snapshot(scope).ApproxBytes

	if after <= before {
		t.Fatalf("reactions must count toward approximate bytes: %d -> %d", before, after)
	}
}

func TestConversationStoreSetMessageReactionsConcurrent(t *testing.T) {
	t.Parallel()

	stores := New(Config{}, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	store := stores.Conversations()

	scope := state.ConversationScope{ChatID: 500}
	store.AppendMessage(scope, state.Message{IsMe: true, MessageID: 50, Text: "answer"})

	var wg sync.WaitGroup
	for userID := int64(1); userID <= 8; userID++ {
		wg.Add(1)
		go func(userID int64) {
			defer wg.Done()
			// One user at a time: each goroutine replaces its own reactions,
			// which is the shape concurrent Telegram updates arrive in.
			for i := 0; i < 50; i++ {
				emoji := "👍"
				if i%2 == 1 {
					emoji = "👎"
				}
				store.SetMessageReactions(500, 50, userID, []state.Reaction{{UserID: userID, Emoji: emoji}})
			}
		}(userID)
	}
	wg.Wait()

	reactions := store.Snapshot(scope).Messages[0].Reactions
	if len(reactions) != 8 {
		t.Fatalf("each user must keep exactly one reaction, got %d: %#v", len(reactions), reactions)
	}
	for _, reaction := range reactions {
		if reaction.Emoji != "👍" && reaction.Emoji != "👎" {
			t.Fatalf("unexpected final reaction: %#v", reaction)
		}
	}
}
