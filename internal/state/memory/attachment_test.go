package memory

import (
	"io"
	"log/slog"
	"testing"

	"telegram-ollama-reply-bot/internal/state"
)

func newAttachmentTestStore() state.ConversationStore {
	return New(Config{
		HistoryStreamsMax:  8,
		HistoryMaxBytes:    1 << 20,
		ImageCacheMaxBytes: 1 << 20,
	}, slog.New(slog.NewJSONHandler(io.Discard, nil))).Conversations()
}

// Snapshots must be isolated copies: mutating a returned attachment can never
// change stored history.
func TestConversationStoreClonesAttachments(t *testing.T) {
	t.Parallel()

	store := newAttachmentTestStore()
	scope := state.ConversationScope{ChatID: 10}

	original := &state.Attachment{Kind: state.AttachmentPoll, Label: `"Lunch?" — Pizza / Sushi`}
	store.AppendMessage(scope, state.Message{Name: "Bot", IsMe: true, MessageID: 5, Attachment: original})

	// The caller's own pointer must not alias stored state either.
	original.Label = "mutated by the caller"

	snapshot := store.Snapshot(scope)
	if len(snapshot.Messages) != 1 || snapshot.Messages[0].Attachment == nil {
		t.Fatalf("attachment lost: %+v", snapshot.Messages)
	}
	if got := snapshot.Messages[0].Attachment.Label; got != `"Lunch?" — Pizza / Sushi` {
		t.Fatalf("stored attachment aliased the caller's pointer: %q", got)
	}

	snapshot.Messages[0].Attachment.Label = "mutated via snapshot"
	again := store.Snapshot(scope)
	if got := again.Messages[0].Attachment.Label; got != `"Lunch?" — Pizza / Sushi` {
		t.Fatalf("snapshot aliased stored state: %q", got)
	}
}

func TestCloneMessageCopiesAttachmentInsideReplyTo(t *testing.T) {
	t.Parallel()

	msg := state.Message{ReplyTo: &state.Message{Attachment: &state.Attachment{Kind: state.AttachmentSticker, Label: "😀"}}}
	cloned := cloneMessage(msg)
	cloned.ReplyTo.Attachment.Label = "changed"

	if msg.ReplyTo.Attachment.Label != "😀" {
		t.Fatal("nested attachment must be deep-copied")
	}
}

func TestMessageApproxBytesCountsAttachment(t *testing.T) {
	t.Parallel()

	plain := state.Message{Text: "hello"}
	withAttachment := plain
	withAttachment.Attachment = &state.Attachment{Kind: state.AttachmentSticker, Label: "a fairly long sticker description"}

	if messageApproxBytes(withAttachment) <= messageApproxBytes(plain) {
		t.Fatalf("attachment must count toward the byte budget: %d <= %d",
			messageApproxBytes(withAttachment), messageApproxBytes(plain))
	}
}

// The label is searchable text, so a model can find "that poll about pizza"
// through the regular history search and get the original message back.
func TestSearchFindsMessagesByAttachmentLabel(t *testing.T) {
	t.Parallel()

	store := newAttachmentTestStore()
	scope := state.ConversationScope{ChatID: 10}
	store.AppendMessage(scope, state.Message{Name: "Alice", Text: "what should we eat?", MessageID: 1})
	store.AppendMessage(scope, state.Message{
		Name: "Bot", IsMe: true, MessageID: 2,
		Attachment: &state.Attachment{Kind: state.AttachmentPoll, Label: `"Dinner?" — Pizza / Sushi`},
	})

	matches := store.Search(scope, state.HistorySearchQuery{
		Keywords:  []string{"sushi"},
		MatchMode: state.HistorySearchMatchModeAny,
		Limit:     5,
	})
	if len(matches) != 1 {
		t.Fatalf("expected exactly the poll message, got %+v", matches)
	}
	got := matches[0].Message
	if got.MessageID != 2 || !got.IsMe || got.Attachment == nil || got.Attachment.Kind != state.AttachmentPoll {
		t.Fatalf("search must return the original message with its attachment: %+v", got)
	}
}
