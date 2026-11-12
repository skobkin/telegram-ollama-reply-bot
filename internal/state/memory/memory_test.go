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
	_ = store.SetEarlierSummary(topicScope, "topic-summary", 1, 0)

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
	_ = store.SetEarlierSummary(scope, "cached summary", 1, 0)
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

	if !store.SetMessageReactions(100, 11, 42, 10, []state.Reaction{{UserID: 42, Name: "Alice", Emoji: "👍"}}) {
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
	if store.SetMessageReactions(100, 10, 42, 11, []state.Reaction{{UserID: 42, Emoji: "👎"}}) {
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

	store.SetMessageReactions(200, 20, 1, 20, []state.Reaction{{UserID: 1, Emoji: "👍"}})
	store.SetMessageReactions(200, 20, 1, 21, []state.Reaction{{UserID: 1, Emoji: "👎"}})
	store.SetMessageReactions(200, 20, 2, 22, []state.Reaction{{UserID: 2, Emoji: "🔥"}})

	reactions := store.Snapshot(scope).Messages[0].Reactions
	if len(reactions) != 2 || reactions[0].UserID != 1 || reactions[0].Emoji != "👎" || reactions[1].UserID != 2 {
		t.Fatalf("per-user replace failed: %#v", reactions)
	}

	// Empty slice removes only that user's reactions.
	if !store.SetMessageReactions(200, 20, 1, 23, nil) {
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

	if store.SetMessageReactions(300, 999, 1, 30, []state.Reaction{{UserID: 1, Emoji: "👍"}}) {
		t.Fatalf("unknown message ID must not be found")
	}
	if store.SetMessageReactions(400, 30, 1, 31, []state.Reaction{{UserID: 1, Emoji: "👍"}}) {
		t.Fatalf("other chat must not be searched")
	}
	if store.SetMessageReactions(300, 0, 1, 32, nil) || store.SetMessageReactions(300, -5, 1, 33, nil) {
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
	store.SetMessageReactions(400, 40, 1, 40, []state.Reaction{{UserID: 1, Name: "Alice", Username: "alice", Emoji: "👍"}})
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
				store.SetMessageReactions(500, 50, userID, int64(1000+i), []state.Reaction{{UserID: userID, Emoji: emoji}})
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

func TestConversationStoreSetMessageReactionsRejectStaleUpdates(t *testing.T) {
	t.Parallel()

	stores := New(Config{}, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	store := stores.Conversations()

	scope := state.ConversationScope{ChatID: 600}
	store.AppendMessage(scope, state.Message{IsMe: true, MessageID: 60, Text: "answer"})

	reaction := func(seq int64, emoji string) bool {
		reactions := []state.Reaction{{UserID: 1, Emoji: emoji}}

		return store.SetMessageReactions(600, 60, 1, seq, reactions)
	}

	if !reaction(5, "👍") {
		t.Fatalf("first update must apply")
	}
	// Same or older sequence is reported as found but not applied.
	if !reaction(5, "👎") || !reaction(3, "👎") {
		t.Fatalf("stale update targets an existing message, so it reports found")
	}
	reactions := store.Snapshot(scope).Messages[0].Reactions
	if len(reactions) != 1 || reactions[0].Emoji != "👍" {
		t.Fatalf("stale updates must not change reactions: %#v", reactions)
	}

	// A newer change applies, including a removal; removals keep the
	// sequence so a later stale addition cannot resurrect the reaction.
	if !reaction(6, "👎") {
		t.Fatalf("newer update must apply")
	}
	if !store.SetMessageReactions(600, 60, 1, 7, nil) {
		t.Fatalf("newer removal must apply")
	}
	if !store.SetMessageReactions(600, 60, 1, 2, []state.Reaction{{UserID: 1, Emoji: "👍"}}) {
		t.Fatalf("stale addition targets an existing message, so it reports found")
	}
	if len(store.Snapshot(scope).Messages[0].Reactions) != 0 {
		t.Fatalf("removed reaction must stay removed")
	}
}

func TestConversationStoreSetMessageReactionsInvalidatesStaleSummary(t *testing.T) {
	t.Parallel()

	stores := New(Config{}, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	store := stores.Conversations()

	scope := state.ConversationScope{ChatID: 700}
	store.AppendMessage(scope, state.Message{IsMe: true, MessageID: 70, Text: "summarized answer"})
	store.AppendMessage(scope, state.Message{IsMe: true, MessageID: 71, Text: "later bot answer"})
	_ = store.SetEarlierSummary(scope, "summary mentioning no reactions", 1, 0)

	// The reacted message sits inside the summarized prefix: the cached
	// summary embeds the old reaction rendering, so it must be dropped.
	if !store.SetMessageReactions(700, 70, 1, 10, []state.Reaction{{UserID: 1, Emoji: "👍"}}) {
		t.Fatalf("expected reaction to apply")
	}
	snapshot := store.Snapshot(scope)
	if snapshot.EarlierSummary != "" || snapshot.SummaryMessageCount != 0 {
		t.Fatalf("summary must be invalidated by a reaction inside the summarized prefix: %#v", snapshot)
	}

	// A reaction outside the summarized prefix leaves the summary intact;
	// the commit must carry the revision current at commit time.
	fresh := store.Snapshot(scope)
	if !store.SetEarlierSummary(scope, "fresh summary", 1, fresh.Revision) {
		t.Fatalf("summary commit against unchanged history must be accepted")
	}
	if !store.SetMessageReactions(700, 71, 2, 11, []state.Reaction{{UserID: 2, Emoji: "👎"}}) {
		t.Fatalf("expected reaction to apply")
	}
	snapshot = store.Snapshot(scope)
	if snapshot.EarlierSummary != "fresh summary" || snapshot.SummaryMessageCount != 1 {
		t.Fatalf("summary must survive a reaction outside the summarized prefix: %#v", snapshot)
	}
}

func TestConversationStoreSummaryCommitRejectedAfterReactionChange(t *testing.T) {
	t.Parallel()

	stores := New(Config{}, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	store := stores.Conversations()

	scope := state.ConversationScope{ChatID: 800}
	store.AppendMessage(scope, state.Message{IsMe: true, MessageID: 80, Text: "answer with reactions"})
	store.AppendMessage(scope, state.Message{Text: "later", FromID: 1, MessageID: 81})

	built := store.Snapshot(scope)
	if !store.SetEarlierSummary(scope, "summary without reactions", 1, built.Revision) {
		t.Fatalf("commit against unchanged history must be accepted")
	}

	// Reaction arrives while the summarizer is awaiting the LLM.
	store.SetMessageReactions(800, 80, 1, 10, []state.Reaction{{UserID: 1, Emoji: "👍"}})

	// The in-flight commit carries the old revision and must be rejected.
	if store.SetEarlierSummary(scope, "stale summary", 2, built.Revision) {
		t.Fatalf("summary commit after a reaction change must be rejected")
	}
	snapshot := store.Snapshot(scope)
	if snapshot.EarlierSummary != "" || snapshot.SummaryMessageCount != 0 {
		t.Fatalf("stale summary must not be restored: %#v", snapshot)
	}

	// A commit based on the post-change snapshot is accepted.
	fresh := store.Snapshot(scope)
	if !store.SetEarlierSummary(scope, "fresh summary", 2, fresh.Revision) {
		t.Fatalf("commit with current revision must be accepted")
	}
	if store.Snapshot(scope).EarlierSummary != "fresh summary" {
		t.Fatalf("accepted summary must be stored")
	}
}

func TestConversationStoreSummaryCommitRejectedAfterReset(t *testing.T) {
	t.Parallel()

	stores := New(Config{}, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	store := stores.Conversations()

	scope := state.ConversationScope{ChatID: 810}
	store.AppendMessage(scope, state.Message{IsMe: true, MessageID: 82, Text: "answer"})
	built := store.Snapshot(scope)

	store.Reset(scope)

	if store.SetEarlierSummary(scope, "summary without history", 1, built.Revision) {
		t.Fatalf("summary commit after reset must be rejected")
	}
	if got := store.Snapshot(scope); len(got.Messages) != 0 || got.EarlierSummary != "" {
		t.Fatalf("reset must not leave a resurrected summary-only stream: %#v", got)
	}
}

func TestConversationStoreReactionSeqRecordsPrunedOnTrimAndCountedInBytes(t *testing.T) {
	t.Parallel()

	stores := New(Config{
		HistoryMaxBytes:    4096,
		HistoryStreamsMax:  16,
		ImageCacheMaxBytes: 1 << 20,
	}, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	store := stores.Conversations()

	scope := state.ConversationScope{ChatID: 820}

	// Enough reacted messages to force head trims under the tiny byte cap.
	const messages = 40
	for i := 1; i <= messages; i++ {
		store.AppendMessage(scope, state.Message{IsMe: true, MessageID: 900 + i, Text: "answer"})
		store.SetMessageReactions(820, 900+i, int64(i), int64(i), []state.Reaction{{UserID: int64(i), Emoji: "👍"}})
	}

	bucket := stores.conversations.streams[scopeKey(scope)]
	if bucket == nil {
		t.Fatalf("expected the stream to survive")
	}
	if len(bucket.reactionSeq) >= messages {
		t.Fatalf("trimmed messages must have their reaction sequence records pruned, got %d records", len(bucket.reactionSeq))
	}
	expectedBytes := int64(len(bucket.reactionSeq)) * reactionSeqRecordApproxBytes
	if bucket.reactionSeqBytes != expectedBytes {
		t.Fatalf("reaction sequence byte accounting must match the record count: %d != %d", bucket.reactionSeqBytes, expectedBytes)
	}
	if bucket.bytes < bucket.reactionSeqBytes {
		t.Fatalf("reaction sequence metadata must count toward bucket bytes")
	}
	if bucket.bytes <= bucket.reactionSeqBytes {
		t.Fatalf("bucket bytes must include message and sequence content: %d", bucket.bytes)
	}

	// The global budget tracks the sequence metadata too.
	snapshot := store.Snapshot(scope)
	if snapshot.ApproxBytes != bucket.bytes {
		t.Fatalf("snapshot approximate bytes must include sequence metadata: %d != %d", snapshot.ApproxBytes, bucket.bytes)
	}
}
