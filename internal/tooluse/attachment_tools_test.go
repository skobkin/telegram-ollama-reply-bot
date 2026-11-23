package tooluse

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"telegram-ollama-reply-bot/internal/state"
	"telegram-ollama-reply-bot/internal/state/memory"
)

func attachmentTestRuntime(history state.ConversationStore) *Runtime {
	return New(&stubLLM{}, history, &stubExtractor{}, nil, &stubActionSender{}, nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)), Config{MaxIterations: 6})
}

func botPollMessage(id int) state.Message {
	return state.Message{
		Name: "Kitsu Bot", Username: "kitsubot", IsMe: true, MessageID: id,
		CreatedAt:  time.Date(2026, 4, 20, 10, 0, 0, 0, time.UTC),
		Attachment: &state.Attachment{Kind: state.AttachmentPoll, Label: `"Dinner?" — Pizza / Sushi`},
	}
}

// A bot-sent poll is found by its content, comes back as the original
// message with its description as text, and is marked as the bot's own.
func TestSearchHistoryReturnsAttachmentMessages(t *testing.T) {
	t.Parallel()

	store := memory.New(memory.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil))).Conversations()
	scope := state.ConversationScope{ChatID: 1}
	store.AppendMessage(scope, state.Message{Name: "alice", Text: "what should we eat?", MessageID: 1})
	store.AppendMessage(scope, botPollMessage(2))

	definition, _ := attachmentTestRuntime(store).registry.Lookup("search_history")
	result, err := definition.Handler(context.Background(), CallContext{Scope: scope}, json.RawMessage(`{"keywords":["sushi"],"match_mode":"any"}`))
	if err != nil || result.Status != "ok" {
		t.Fatalf("search failed: %+v %v", result, err)
	}

	matches := result.Data.(map[string]any)["matches"].([]map[string]any)
	if len(matches) != 1 {
		t.Fatalf("expected the poll only, got %+v", matches)
	}
	got := matches[0]
	if got["message_id"] != 2 || got["is_me"] != true || got["attachment_kind"] != "poll" {
		t.Fatalf("match = %+v", got)
	}
	if text, _ := got["text"].(string); !strings.Contains(text, `[Poll: "Dinner?" — Pizza / Sushi]`) {
		t.Fatalf("an attachment-only message must not have empty text: %q", text)
	}
}

func TestSearchHistoryOmitsAttachmentKindForTextMessages(t *testing.T) {
	t.Parallel()

	store := memory.New(memory.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil))).Conversations()
	scope := state.ConversationScope{ChatID: 1}
	store.AppendMessage(scope, state.Message{Name: "alice", Text: "pizza night", MessageID: 1})

	definition, _ := attachmentTestRuntime(store).registry.Lookup("search_history")
	result, _ := definition.Handler(context.Background(), CallContext{Scope: scope}, json.RawMessage(`{"keywords":["pizza"]}`))

	matches := result.Data.(map[string]any)["matches"].([]map[string]any)
	if len(matches) != 1 {
		t.Fatalf("matches = %+v", matches)
	}
	if _, present := matches[0]["attachment_kind"]; present {
		t.Fatalf("plain text messages carry no attachment_kind: %+v", matches[0])
	}
	if matches[0]["is_me"] != false {
		t.Fatalf("is_me = %v", matches[0]["is_me"])
	}
}

func TestThreadMessageViewShowsAttachments(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		msg      state.Message
		wantText string
		wantKind any
	}{
		{"attachment only", state.Message{MessageID: 1, Attachment: &state.Attachment{Kind: state.AttachmentSticker, Label: "😀"}}, "[Sticker: 😀]", "sticker"},
		{"attachment with caption", state.Message{MessageID: 2, Text: "listen to this", Attachment: &state.Attachment{Kind: state.AttachmentVoice, Label: "0:12"}}, "[Voice: 0:12] listen to this", "voice"},
		{"plain text", state.Message{MessageID: 3, Text: "hello"}, "hello", nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			view := threadMessageView(tt.msg)
			if view["text_preview"] != tt.wantText {
				t.Fatalf("text_preview = %q, want %q", view["text_preview"], tt.wantText)
			}
			if got := view["attachment_kind"]; got != tt.wantKind {
				t.Fatalf("attachment_kind = %v, want %v", got, tt.wantKind)
			}
		})
	}
}

func TestThreadMessageHelpersCarryAttachments(t *testing.T) {
	t.Parallel()

	withAttachment := state.Message{MessageID: 5, Attachment: &state.Attachment{Kind: state.AttachmentChart, Label: "bar: Votes"}}

	// Clones are independent copies.
	cloned := cloneThreadMessage(withAttachment)
	cloned.Attachment.Label = "changed"
	if withAttachment.Attachment.Label != "bar: Votes" {
		t.Fatal("cloneThreadMessage must deep-copy the attachment")
	}

	// A merge fills a missing attachment from the other copy (for example a
	// quoted copy embedded in a reply) and never aliases it.
	merged := mergeThreadMessage(state.Message{MessageID: 5, Name: "Bot"}, withAttachment)
	if merged.Attachment == nil || merged.Attachment.Kind != state.AttachmentChart {
		t.Fatalf("merge lost the attachment: %+v", merged)
	}
	merged.Attachment.Label = "changed"
	if withAttachment.Attachment.Label != "bar: Votes" {
		t.Fatal("mergeThreadMessage must not alias the source attachment")
	}

	// An entry whose only content is an attachment is not empty.
	if isEmptyThreadMessage(state.Message{Attachment: &state.Attachment{Kind: state.AttachmentDice}}) {
		t.Fatal("an attachment-only message must not be treated as empty")
	}
}

// End to end through the thread tool: a user replying to a bot poll sees the
// poll as the reply target, with its description.
func TestGetMessageThreadContextShowsTheRepliedToBotPoll(t *testing.T) {
	t.Parallel()

	store := memory.New(memory.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil))).Conversations()
	scope := state.ConversationScope{ChatID: 1}
	poll := botPollMessage(2)
	current := state.Message{
		Name: "alice", Username: "alice", Text: "pizza!", MessageID: 3, FromID: 10, IsUserRequest: true,
		ReplyTo:   &poll,
		CreatedAt: poll.CreatedAt.Add(time.Minute),
	}
	store.AppendMessage(scope, poll)
	store.AppendMessage(scope, current)

	definition, _ := attachmentTestRuntime(store).registry.Lookup("get_message_thread_context")
	result, err := definition.Handler(context.Background(), CallContext{Scope: scope, Requester: current}, json.RawMessage(`{}`))
	if err != nil || result.Status != "ok" {
		t.Fatalf("thread context failed: %+v %v", result, err)
	}

	raw, err := json.Marshal(result.Data)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`[Poll: \"Dinner?\" — Pizza / Sushi]`, `"attachment_kind":"poll"`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("thread context lacks %s: %s", want, raw)
		}
	}
}
