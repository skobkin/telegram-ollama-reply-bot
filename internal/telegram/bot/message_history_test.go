package bot

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"telegram-ollama-reply-bot/internal/config"
	"telegram-ollama-reply-bot/internal/llm"
	"telegram-ollama-reply-bot/internal/state"
	"telegram-ollama-reply-bot/internal/state/memory"

	tg "github.com/mymmrac/telego"
)

// mutatingSummarizer applies a reaction change to history while the LLM call
// is in flight, reproducing the concurrent-modification window of history
// summarization deterministically.
type mutatingSummarizer struct {
	history state.ConversationStore
	mutate  func()
}

func (s *mutatingSummarizer) Summarize(context.Context, llm.PromptScope, string, string) (string, *llm.TokenUsage, error) {
	s.mutate()

	return "summary built from old reactions", &llm.TokenUsage{}, nil
}

// TestMaybeSummarizeHistoryDiscardsSummaryAfterReactionChange proves the
// summarizer's commit is rejected when the raw history changed during
// generation: a reaction change on a summarized message must not be
// papered over by a stale in-flight summary.
func TestMaybeSummarizeHistoryDiscardsSummaryAfterReactionChange(t *testing.T) {
	store := memory.New(memory.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil))).Conversations()

	// limit=2, threshold=1: summarization runs once unsummarized > 3.
	cfg := config.BotConfig{UncompressedHistoryLimit: 2, HistorySummaryThreshold: 1}
	testBot := &Bot{
		ctx:     context.Background(),
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		cfg:     cfg,
		history: store,
		stats:   &dmStatsSpy{},
		llm: &mutatingSummarizer{history: store, mutate: func() {
			store.SetMessageReactions(100500, 11, 42, 100, []state.Reaction{{UserID: 42, Emoji: "👍"}})
		}},
	}

	scope := state.ConversationScope{ChatID: 100500}
	store.AppendMessage(scope, state.Message{IsMe: true, MessageID: 11, Text: "summarized answer"})
	for i := 0; i < 4; i++ {
		store.AppendMessage(scope, state.Message{Text: "filler", FromID: 1, MessageID: 20 + i})
	}

	message := tg.Message{MessageID: 24, Chat: tg.Chat{ID: 100500, Type: tg.ChatTypePrivate}}
	testBot.maybeSummarizeHistory(context.Background(), message)

	snapshot := store.Snapshot(scope)
	if snapshot.EarlierSummary != "" || snapshot.SummaryMessageCount != 0 {
		t.Fatalf("stale in-flight summary must be discarded, got: %#v", snapshot)
	}
	if len(snapshot.Messages[0].Reactions) != 1 {
		t.Fatalf("applied reaction must remain on the raw message: %#v", snapshot.Messages[0])
	}
}

// TestMaybeSummarizeHistoryCommitsWithoutInterveningChange proves the normal
// path still commits: with no history change during generation the summary
// is stored with its revision intact.
func TestMaybeSummarizeHistoryCommitsWithoutInterveningChange(t *testing.T) {
	store := memory.New(memory.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil))).Conversations()

	cfg := config.BotConfig{UncompressedHistoryLimit: 2, HistorySummaryThreshold: 1}
	testBot := &Bot{
		ctx:     context.Background(),
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		cfg:     cfg,
		history: store,
		stats:   &dmStatsSpy{},
		llm:     &mutatingSummarizer{history: store, mutate: func() {}},
	}

	scope := state.ConversationScope{ChatID: 100500}
	store.AppendMessage(scope, state.Message{IsMe: true, MessageID: 11, Text: "summarized answer"})
	for i := 0; i < 4; i++ {
		store.AppendMessage(scope, state.Message{Text: "filler", FromID: 1, MessageID: 20 + i})
	}

	message := tg.Message{MessageID: 24, Chat: tg.Chat{ID: 100500, Type: tg.ChatTypePrivate}}
	testBot.maybeSummarizeHistory(context.Background(), message)

	snapshot := store.Snapshot(scope)
	if snapshot.EarlierSummary != "summary built from old reactions" || snapshot.SummaryMessageCount == 0 {
		t.Fatalf("unchanged history must accept the summary commit, got: %#v", snapshot)
	}
}
