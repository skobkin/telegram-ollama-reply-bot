package llmcontext

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"telegram-ollama-reply-bot/internal/state"
	"telegram-ollama-reply-bot/internal/state/memory"
)

func TestReplyBuilderUsesSameTopicHistoryOnly(t *testing.T) {
	t.Parallel()

	stores := memory.New(memory.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	builder := NewReplyBuilder(stores.Conversations(), func(_ context.Context, messages []state.Message) []state.Message {
		return messages
	}, 15, 100)

	topicScope := state.ConversationScope{ChatID: 10, TopicID: 1}
	otherScope := state.ConversationScope{ChatID: 10, TopicID: 2}

	stores.Conversations().AppendMessage(topicScope, state.Message{
		Name:          "Alice",
		Username:      "alice",
		Text:          "topic one",
		IsUserRequest: true,
		ChatID:        topicScope.ChatID,
		TopicID:       topicScope.TopicID,
		CreatedAt:     time.Now().UTC(),
	})
	stores.Conversations().AppendMessage(otherScope, state.Message{
		Name:          "Bob",
		Username:      "bob",
		Text:          "topic two",
		IsUserRequest: true,
		ChatID:        otherScope.ChatID,
		TopicID:       otherScope.TopicID,
		CreatedAt:     time.Now().UTC(),
	})
	_ = stores.Conversations().SetEarlierSummary(topicScope, "same topic summary", 1, 0)
	_ = stores.Conversations().SetEarlierSummary(otherScope, "wrong topic summary", 1, 0)

	ctx := builder.BuildReplyContext(context.Background(), ReplyInput{
		Chat: ChatContext{
			Title: "Example",
			Type:  "supergroup",
		},
		User: UserContext{
			FirstName: "Alice",
			Username:  "alice",
		},
		Scope: topicScope,
		CurrentMessage: state.Message{
			Name:          "Alice",
			Username:      "alice",
			Text:          "current message",
			IsUserRequest: true,
		},
		Trigger: TriggerMention,
	})

	if len(ctx.History) != 0 {
		t.Fatalf("expected summarized history to be omitted from verbatim prompt, got %d messages", len(ctx.History))
	}
	if ctx.EarlierSummary != "same topic summary" {
		t.Fatalf("unexpected earlier summary: %q", ctx.EarlierSummary)
	}
	if !strings.Contains(ctx.SystemHint, "Use only this topic's context") {
		t.Fatalf("expected topic hint in system context: %q", ctx.SystemHint)
	}
}

func TestReplyBuilderRendersCompactHistoryAndHydratesImages(t *testing.T) {
	t.Parallel()

	stores := memory.New(memory.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	builder := NewReplyBuilder(stores.Conversations(), func(_ context.Context, messages []state.Message) []state.Message {
		result := make([]state.Message, len(messages))
		copy(result, messages)
		for i := range result {
			if result[i].HasImage && result[i].Image == "" {
				result[i].Image = "hydrated image"
			}
		}

		return result
	}, 15, 100)

	scope := state.ConversationScope{ChatID: 10}
	stores.Conversations().AppendMessage(scope, state.Message{
		Name:          "Alice",
		Username:      "alice",
		Text:          "look at this",
		IsUserRequest: true,
		HasImage:      true,
		ReplyTo: &state.Message{
			Name:     "Bob",
			Username: "bob",
			Text:     "older message",
		},
		ChatID:    scope.ChatID,
		CreatedAt: time.Now().UTC(),
	})

	ctx := builder.BuildReplyContext(context.Background(), ReplyInput{
		Chat: ChatContext{Type: "group"},
		User: UserContext{
			FirstName: "Alice",
			LastName:  "Example",
			Username:  "alice",
		},
		Scope: scope,
		CurrentMessage: state.Message{
			Name:          "Alice",
			Username:      "alice",
			Text:          "new image",
			IsUserRequest: true,
			HasImage:      true,
		},
		Trigger: TriggerReply,
	})

	if got := ctx.History[0].Text(); !strings.Contains(got, "> Bob (@bob): older message") {
		t.Fatalf("expected reply snippet, got %q", got)
	}
	if got := ctx.History[0].Text(); !strings.Contains(got, "[Image: hydrated image]") {
		t.Fatalf("expected hydrated image marker in history, got %q", got)
	}
	if got := ctx.UserMessage.Text(); !strings.Contains(got, "[Image: hydrated image]") {
		t.Fatalf("expected hydrated image marker in user message, got %q", got)
	}
	if !strings.Contains(ctx.SystemHint, "Trigger: reply.") {
		t.Fatalf("expected trigger hint, got %q", ctx.SystemHint)
	}
	if !strings.Contains(ctx.SystemHint, "Username: @alice.") {
		t.Fatalf("expected compact user hint, got %q", ctx.SystemHint)
	}
}

func TestRenderMessagesPlainTextUsesCompactFormat(t *testing.T) {
	t.Parallel()

	text := RenderMessagesPlainText([]state.Message{
		{
			Name:     "Alice",
			Username: "alice",
			Text:     "main",
			ReplyTo: &state.Message{
				Name: "Bob",
				Text: "quoted",
			},
		},
	})

	if !strings.Contains(text, "> Bob: quoted") {
		t.Fatalf("expected compact quoted text, got %q", text)
	}
	if !strings.Contains(text, "Alice (@alice): main") {
		t.Fatalf("expected compact user text, got %q", text)
	}
}

func TestReplyBuilderUsesRecentLimitWithoutSummary(t *testing.T) {
	t.Parallel()

	stores := memory.New(memory.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	builder := NewReplyBuilder(stores.Conversations(), func(_ context.Context, messages []state.Message) []state.Message {
		return messages
	}, 2, 100)

	scope := state.ConversationScope{ChatID: 10}
	for _, text := range []string{"one", "two", "three"} {
		stores.Conversations().AppendMessage(scope, state.Message{
			Name:      "Alice",
			Text:      text,
			CreatedAt: time.Now().UTC(),
		})
	}

	ctx := builder.BuildReplyContext(context.Background(), ReplyInput{
		Chat:           ChatContext{Type: "group"},
		Scope:          scope,
		CurrentMessage: state.Message{Name: "Alice", Text: "current"},
	})

	if len(ctx.History) != 2 {
		t.Fatalf("expected 2 recent history messages, got %d", len(ctx.History))
	}
	if got := ctx.History[0].Text(); !strings.Contains(got, "two") {
		t.Fatalf("unexpected first recent message: %q", got)
	}
	if got := ctx.History[1].Text(); !strings.Contains(got, "three") {
		t.Fatalf("unexpected second recent message: %q", got)
	}
}

func TestReplyBuilderUsesUnsummarizedTailWhenSummaryExists(t *testing.T) {
	t.Parallel()

	stores := memory.New(memory.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	builder := NewReplyBuilder(stores.Conversations(), func(_ context.Context, messages []state.Message) []state.Message {
		return messages
	}, 2, 100)

	scope := state.ConversationScope{ChatID: 10}
	for _, text := range []string{"one", "two", "three", "four"} {
		stores.Conversations().AppendMessage(scope, state.Message{
			Name:      "Alice",
			Text:      text,
			CreatedAt: time.Now().UTC(),
		})
	}
	_ = stores.Conversations().SetEarlierSummary(scope, "summary of one and two", 2, 0)

	ctx := builder.BuildReplyContext(context.Background(), ReplyInput{
		Chat:           ChatContext{Type: "group"},
		Scope:          scope,
		CurrentMessage: state.Message{Name: "Alice", Text: "current"},
	})

	if ctx.EarlierSummary != "summary of one and two" {
		t.Fatalf("unexpected earlier summary: %q", ctx.EarlierSummary)
	}
	if len(ctx.History) != 2 {
		t.Fatalf("expected unsummarized tail only, got %d messages", len(ctx.History))
	}
	if got := ctx.History[0].Text(); !strings.Contains(got, "three") {
		t.Fatalf("unexpected first tail message: %q", got)
	}
	if got := ctx.History[1].Text(); !strings.Contains(got, "four") {
		t.Fatalf("unexpected second tail message: %q", got)
	}
}

// TestReplyBuilderBoundsRawTailDuringPartialSummary proves the raw tail is
// bounded while a partial summary is committed mid-rebuild: a cleared-summary
// chunk rebuild leaves a long unsummarized middle, which must not be
// forwarded to the reply model wholesale.
func TestReplyBuilderBoundsRawTailDuringPartialSummary(t *testing.T) {
	t.Parallel()

	stores := memory.New(memory.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	builder := NewReplyBuilder(stores.Conversations(), func(_ context.Context, messages []state.Message) []state.Message {
		return messages
	}, 15, 4)

	scope := state.ConversationScope{ChatID: 10}
	for _, text := range []string{"one", "two", "three", "four", "five", "six", "seven", "eight", "nine", "ten"} {
		stores.Conversations().AppendMessage(scope, state.Message{
			Name:      "Alice",
			Text:      text,
			CreatedAt: time.Now().UTC(),
		})
	}
	// Mid-rebuild state: the summary covers only the first two messages,
	// leaving an eight-message raw middle.
	_ = stores.Conversations().SetEarlierSummary(scope, "partial summary", 2, 0)

	ctx := builder.BuildReplyContext(context.Background(), ReplyInput{
		Chat:           ChatContext{Type: "group"},
		Scope:          scope,
		CurrentMessage: state.Message{Name: "Alice", Text: "current"},
	})

	if ctx.EarlierSummary != "partial summary" {
		t.Fatalf("unexpected earlier summary: %q", ctx.EarlierSummary)
	}
	if len(ctx.History) != 4 {
		t.Fatalf("raw tail must be bounded during rebuild, got %d messages", len(ctx.History))
	}
	if got := ctx.History[0].Text(); !strings.Contains(got, "seven") {
		t.Fatalf("bounded tail must keep the newest messages, got first %q", got)
	}

	// Normal summary state is untouched: a tail within the bound forwards
	// every unsummarized message, as before.
	bounded := NewReplyBuilder(stores.Conversations(), func(_ context.Context, messages []state.Message) []state.Message {
		return messages
	}, 15, 25)
	ctx = bounded.BuildReplyContext(context.Background(), ReplyInput{
		Chat:           ChatContext{Type: "group"},
		Scope:          scope,
		CurrentMessage: state.Message{Name: "Alice", Text: "current"},
	})
	if len(ctx.History) != 8 {
		t.Fatalf("summary state within the bound must forward the whole tail, got %d messages", len(ctx.History))
	}
}

// TestReplyBuilderKeepsFullHistoryWhenSummarizationDisabled proves the
// summary-tail cap never applies without a summary: with summarization
// disabled (limit 0) the full history must stay available, because nothing
// omitted here ever returns through a summary.
func TestReplyBuilderKeepsFullHistoryWhenSummarizationDisabled(t *testing.T) {
	t.Parallel()

	stores := memory.New(memory.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	builder := NewReplyBuilder(stores.Conversations(), func(_ context.Context, messages []state.Message) []state.Message {
		return messages
	}, 0, 10)

	scope := state.ConversationScope{ChatID: 10}
	for i := 0; i < 20; i++ {
		stores.Conversations().AppendMessage(scope, state.Message{
			Name:      "Alice",
			Text:      fmt.Sprintf("message %d", i),
			CreatedAt: time.Now().UTC(),
		})
	}

	ctx := builder.BuildReplyContext(context.Background(), ReplyInput{
		Chat:           ChatContext{Type: "group"},
		Scope:          scope,
		CurrentMessage: state.Message{Name: "Alice", Text: "current"},
	})

	if len(ctx.History) != 20 {
		t.Fatalf("disabled summarization must keep the full history, got %d of 20 messages", len(ctx.History))
	}
}

func TestAssistantMessagesRenderReactionsForModel(t *testing.T) {
	t.Parallel()

	message := state.Message{
		Text: "here is the answer",
		IsMe: true,
		Reactions: []state.Reaction{
			{UserID: 1, Name: "Alice", Emoji: "👍"},
			{UserID: 2, Name: "", Username: "bob", Emoji: "👎"},
			{UserID: 3, Name: "", Username: "", Emoji: "🔥"},
		},
	}

	rendered := renderAssistantMessage(message)
	if !strings.HasSuffix(rendered, "\n[reactions: 👍 Alice, 👎 @bob, 🔥 user #3]") {
		t.Fatalf("expected reaction suffix with name fallbacks, got %q", rendered)
	}
}

func TestPlainTextRenderingIncludesBotReactions(t *testing.T) {
	t.Parallel()

	text := RenderMessagesPlainText([]state.Message{
		{IsMe: true, Text: "bot answer", Reactions: []state.Reaction{
			{UserID: 7, Name: "Alice", Emoji: "👎"},
		}},
	})

	if !strings.Contains(text, "👎 Alice") {
		t.Fatalf("expected reactions to survive plain-text rendering for summaries, got %q", text)
	}
}

func TestNoReactionsRendersNoSuffix(t *testing.T) {
	t.Parallel()

	if strings.Contains(renderAssistantMessage(state.Message{Text: "plain", IsMe: true}), "[reactions") {
		t.Fatalf("no reaction suffix expected without reactions")
	}
	if strings.Contains(presentMessage(state.Message{Name: "A", Text: "x"}), "[reactions") {
		t.Fatalf("no reaction suffix expected without reactions")
	}
}
