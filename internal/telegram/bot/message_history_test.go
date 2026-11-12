package bot

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
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

// countingSummarizer records each summarize input so request chunking and
// folding can be observed from the test.
type countingSummarizer struct {
	inputs []string
}

func (s *countingSummarizer) Summarize(_ context.Context, _ llm.PromptScope, prompt, _ string) (string, *llm.TokenUsage, error) {
	s.inputs = append(s.inputs, prompt)

	return "chunk summary", &llm.TokenUsage{}, nil
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

// TestMaybeSummarizeHistoryRebuildsClearedSummaryInBoundedChunks proves a
// cleared summary (a reaction on an already-summarized message) is rebuilt in
// threshold-sized chunks: no single summarize request carries the whole
// retained prefix, chunk progress is committed as it goes, and the rebuilt
// input includes the reaction feedback that triggered the rebuild.
func TestMaybeSummarizeHistoryRebuildsClearedSummaryInBoundedChunks(t *testing.T) {
	store := memory.New(memory.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil))).Conversations()

	summarizer := &countingSummarizer{}
	cfg := config.BotConfig{UncompressedHistoryLimit: 2, HistorySummaryThreshold: 1}
	testBot := &Bot{
		ctx:     context.Background(),
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		cfg:     cfg,
		history: store,
		stats:   &dmStatsSpy{},
		llm:     summarizer,
	}

	scope := state.ConversationScope{ChatID: 100500}
	// One bot message plus seven fillers: end = 8 - 2 = 6 chunked messages.
	store.AppendMessage(scope, state.Message{IsMe: true, MessageID: 11, Text: "summarized answer"})
	for i := 0; i < 7; i++ {
		store.AppendMessage(scope, state.Message{Text: fmt.Sprintf("filler message number %d", i), FromID: 1, MessageID: 20 + i})
	}

	// A summary covering the first five messages goes stale when a reaction
	// lands on the second message: the store clears it for a raw rebuild.
	_ = store.SetEarlierSummary(scope, "old summary", 5, store.Snapshot(scope).Revision)
	if !store.SetMessageReactions(100500, 11, 42, 10, []state.Reaction{{UserID: 42, Emoji: "👍"}}) {
		t.Fatalf("expected the reaction to land on the summarized bot message")
	}
	if snapshot := store.Snapshot(scope); snapshot.EarlierSummary != "" {
		t.Fatalf("expected the reaction to clear the summary, got %q", snapshot.EarlierSummary)
	}

	message := tg.Message{MessageID: 26, Chat: tg.Chat{ID: 100500, Type: tg.ChatTypePrivate}}
	testBot.maybeSummarizeHistory(context.Background(), message)

	if len(summarizer.inputs) < 2 {
		t.Fatalf("rebuild must be chunked into multiple bounded requests, got %d calls", len(summarizer.inputs))
	}
	for i, input := range summarizer.inputs {
		// chunkSize = threshold = 1: no single request may carry two raw
		// history messages, however long the retained prefix is.
		if got := strings.Count(input, "filler message"); got > 1 {
			t.Fatalf("chunk %d carried %d raw messages in one request: %q", i, got, input)
		}
	}
	if !strings.Contains(summarizer.inputs[0], "reactions:") {
		t.Fatalf("rebuilt input must include the reaction feedback: %q", summarizer.inputs[0])
	}

	snapshot := store.Snapshot(scope)
	if snapshot.SummaryMessageCount != 6 || snapshot.EarlierSummary != "chunk summary" {
		t.Fatalf("chunk progress must be committed, got: %#v", snapshot)
	}
}

// TestMaybeSummarizeHistorySingleRequestForOrdinaryCrossing proves ordinary
// threshold crossings keep their single summarize request: at the default
// window (21 unsummarized over limit 15, threshold 5) the six accumulated
// messages are summarized in one call, not split into a chunk pair.
func TestMaybeSummarizeHistorySingleRequestForOrdinaryCrossing(t *testing.T) {
	store := memory.New(memory.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil))).Conversations()

	summarizer := &countingSummarizer{}
	cfg := config.BotConfig{UncompressedHistoryLimit: 15, HistorySummaryThreshold: 5}
	testBot := &Bot{
		ctx:     context.Background(),
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		cfg:     cfg,
		history: store,
		stats:   &dmStatsSpy{},
		llm:     summarizer,
	}

	scope := state.ConversationScope{ChatID: 100500}
	// 21 unsummarized messages cross the default window by exactly one —
	// the shape of every ordinary trigger.
	for i := 0; i < 21; i++ {
		store.AppendMessage(scope, state.Message{Text: fmt.Sprintf("filler message number %d", i), FromID: 1, MessageID: 100 + i})
	}

	message := tg.Message{MessageID: 120, Chat: tg.Chat{ID: 100500, Type: tg.ChatTypePrivate}}
	testBot.maybeSummarizeHistory(context.Background(), message)

	if len(summarizer.inputs) != 1 {
		t.Fatalf("ordinary crossing must be one summarize request, got %d calls", len(summarizer.inputs))
	}
	if got := strings.Count(summarizer.inputs[0], "filler message"); got != 6 {
		t.Fatalf("expected the six accumulated messages in one request, got %d: %q", got, summarizer.inputs[0])
	}
	if snapshot := store.Snapshot(scope); snapshot.SummaryMessageCount != 6 || snapshot.EarlierSummary != "chunk summary" {
		t.Fatalf("unexpected commit state: %#v", snapshot)
	}

	// The next ordinary trigger (five messages later) is again a single
	// request folding the previous summary, not a re-split of the window.
	for i := 21; i < 27; i++ {
		store.AppendMessage(scope, state.Message{Text: fmt.Sprintf("filler message number %d", i), FromID: 1, MessageID: 100 + i})
	}
	testBot.maybeSummarizeHistory(context.Background(), message)

	if len(summarizer.inputs) != 2 {
		t.Fatalf("next ordinary crossing must also be one request, got %d calls total", len(summarizer.inputs))
	}
	second := summarizer.inputs[1]
	if !strings.Contains(second, "Earlier conversation summary:") {
		t.Fatalf("second request must fold the previous summary: %q", second)
	}
	if got := strings.Count(second, "filler message"); got != 6 {
		t.Fatalf("second request must carry only the new messages, got %d: %q", got, second)
	}
	if snapshot := store.Snapshot(scope); snapshot.SummaryMessageCount != 12 {
		t.Fatalf("unexpected second commit state: %#v", snapshot)
	}
}
