package llmcontext

import (
	"context"
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
	}, 15)

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
	stores.Conversations().SetEarlierSummary(topicScope, "same topic summary", 1)
	stores.Conversations().SetEarlierSummary(otherScope, "wrong topic summary", 1)

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
	}, 15)

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
	}, 2)

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
	}, 2)

	scope := state.ConversationScope{ChatID: 10}
	for _, text := range []string{"one", "two", "three", "four"} {
		stores.Conversations().AppendMessage(scope, state.Message{
			Name:      "Alice",
			Text:      text,
			CreatedAt: time.Now().UTC(),
		})
	}
	stores.Conversations().SetEarlierSummary(scope, "summary of one and two", 2)

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
