package llmcontext

import (
	"context"
	"testing"
	"time"

	"telegram-ollama-reply-bot/internal/state"
)

type snapshotStore struct {
	state.ConversationStore
	snapshot state.ConversationSnapshot
	calls    int
	scope    state.ConversationScope
}

func (s *snapshotStore) Snapshot(scope state.ConversationScope) state.ConversationSnapshot {
	s.calls++
	s.scope = scope

	return s.snapshot
}

func TestReplyMetadataUsesExactContextSnapshot(t *testing.T) {
	start := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	store := &snapshotStore{snapshot: state.ConversationSnapshot{EarlierSummary: "earlier", SummaryMessageCount: 2}}
	for i := 0; i < 5; i++ {
		store.snapshot.Messages = append(store.snapshot.Messages, state.Message{MessageID: i + 1, Text: "retained", CreatedAt: start.Add(time.Duration(i) * time.Hour)})
	}
	builder := NewReplyBuilder(store, func(_ context.Context, messages []state.Message) []state.Message { return messages }, 2, 1)
	scope := state.ConversationScope{ChatID: 10, TopicID: 7}
	reply := builder.BuildReplyContext(context.Background(), ReplyInput{Scope: scope, CurrentMessage: state.Message{Text: "trigger"}})
	metadata := reply.RetainedHistory
	if store.calls != 1 || store.scope != scope || metadata == nil || metadata.RetainedMessages != 5 || len(reply.History) != 1 || !metadata.OldestMessageAt.Equal(start) || !metadata.NewestMessageAt.Equal(start.Add(4*time.Hour)) || reply.EarlierSummary != "earlier" {
		t.Fatalf("reply=%+v metadata=%+v calls=%d", reply, metadata, store.calls)
	}
	store.snapshot.Messages = nil
	if metadata.RetainedMessages != 5 || len(reply.History) != 1 {
		t.Fatal("captured context changed with store")
	}
}

func TestRetainedMetadataEmptyMissingAndOutOfOrderTimes(t *testing.T) {
	now := time.Now().UTC()
	empty := retainedMetadata(nil)
	if empty.RetainedMessages != 0 || !empty.OldestMessageAt.IsZero() {
		t.Fatal(empty)
	}
	partial := retainedMetadata([]state.Message{{CreatedAt: now}, {}})
	if partial.RetainedMessages != 2 || !partial.OldestMessageAt.IsZero() || !partial.NewestMessageAt.IsZero() {
		t.Fatal(partial)
	}
	unordered := retainedMetadata([]state.Message{{CreatedAt: now}, {CreatedAt: now.Add(-time.Hour)}})
	if !unordered.OldestMessageAt.Equal(now.Add(-time.Hour)) || !unordered.NewestMessageAt.Equal(now) {
		t.Fatal(unordered)
	}
}
