package llmcontext

import (
	"strings"
	"testing"

	"telegram-ollama-reply-bot/internal/llm"
	"telegram-ollama-reply-bot/internal/state"
)

func pollAttachment() *state.Attachment {
	return &state.Attachment{Kind: state.AttachmentPoll, Label: `"Dinner?" — Pizza / Sushi`}
}

// A bot-sent poll or sticker has no text. It must still reach the model as a
// non-empty assistant message, or the history would silently drop the bot's
// own action (and some backends reject empty messages).
func TestAssistantAttachmentMessageIsNeverEmpty(t *testing.T) {
	t.Parallel()

	message := messageToLLM(state.Message{IsMe: true, Name: "Bot", Attachment: pollAttachment()})

	if message.Role != llm.RoleAssistant {
		t.Fatalf("role = %s", message.Role)
	}
	if got := message.Text(); !strings.Contains(got, `(sent a poll via send_poll: "Dinner?" — Pizza / Sushi)`) {
		t.Fatalf("assistant message = %q", got)
	}
}

// The bot's own media must not appear in assistant turns as a bracketed
// placeholder: the model copies the format of its earlier turns and would write
// `[Sticker: …]` as text instead of calling send_sticker.
func TestAssistantMediaIsNarratedNotBracketed(t *testing.T) {
	t.Parallel()

	got := renderAssistantMessage(state.Message{
		IsMe: true, Name: "Bot", Text: "enjoy",
		Attachment: &state.Attachment{Kind: state.AttachmentSticker, Label: "😀 (cat waving)"},
	})
	if got != "(sent a sticker via send_sticker: 😀 (cat waving)) enjoy" {
		t.Fatalf("assistant = %q", got)
	}
	if strings.Contains(got, "[Sticker") {
		t.Fatalf("assistant turn must not contain a sticker placeholder: %q", got)
	}
}

func TestUserAttachmentMessageKeepsSenderAndCaption(t *testing.T) {
	t.Parallel()

	got := renderMessagePlainText(state.Message{
		Name: "Alice", Username: "alice", Text: "listen",
		Attachment: &state.Attachment{Kind: state.AttachmentVoice, Label: "0:12"},
	})
	if got != "Alice (@alice): [Voice: 0:12] listen" {
		t.Fatalf("rendered = %q", got)
	}

	onlySticker := renderMessagePlainText(state.Message{Name: "Alice", Attachment: &state.Attachment{Kind: state.AttachmentSticker, Label: "😀 (catpack)"}})
	if !strings.Contains(onlySticker, "[Sticker: 😀 (catpack)]") {
		t.Fatalf("rendered = %q", onlySticker)
	}
}

// Messages without attachments render exactly as before.
func TestMessagesWithoutAttachmentsRenderUnchanged(t *testing.T) {
	t.Parallel()

	if got := renderMessagePlainText(state.Message{Name: "Alice", Text: "hi"}); got != "Alice: hi" {
		t.Fatalf("user = %q", got)
	}
	if got := renderAssistantMessage(state.Message{IsMe: true, Text: "hello"}); got != "hello" {
		t.Fatalf("assistant = %q", got)
	}
}

// Summarization reads history through the plain-text renderer, so the bot's
// poll is visible to the summary instead of vanishing from the transcript.
func TestSummarizationTranscriptIncludesAttachments(t *testing.T) {
	t.Parallel()

	transcript := RenderMessagesPlainText([]state.Message{
		{Name: "Alice", Text: "what should we eat?"},
		{Name: "Bot", IsMe: true, Attachment: pollAttachment()},
		{Name: "Alice", Attachment: &state.Attachment{Kind: state.AttachmentSticker, Label: "👍"}},
	})

	for _, want := range []string{"Alice: what should we eat?", `Bot: [Poll: "Dinner?" — Pizza / Sushi]`, "Alice: [Sticker: 👍]"} {
		if !strings.Contains(transcript, want) {
			t.Errorf("transcript lacks %q:\n%s", want, transcript)
		}
	}
}

// A reply to an attachment message quotes it with its description.
func TestQuotedAttachmentIsRendered(t *testing.T) {
	t.Parallel()

	poll := state.Message{Name: "Bot", IsMe: true, Attachment: pollAttachment()}
	got := renderMessagePlainText(state.Message{Name: "Alice", Text: "pizza!", ReplyTo: &poll})

	if !strings.Contains(got, `> Bot: [Poll: "Dinner?" — Pizza / Sushi]`) || !strings.HasSuffix(got, "Alice: pizza!") {
		t.Fatalf("rendered = %q", got)
	}
}
