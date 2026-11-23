package state

import "testing"

func TestAttachmentPlaceholder(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		attachment *Attachment
		want       string
	}{
		{"nil attachment renders nothing", nil, ""},
		{"empty kind renders nothing", &Attachment{Label: "orphan label"}, ""},
		{"label", &Attachment{Kind: AttachmentSticker, Label: "😀 cat waving"}, "[Sticker: 😀 cat waving]"},
		{"no label", &Attachment{Kind: AttachmentPoll}, "[Poll]"},
		{"multi-word kind", &Attachment{Kind: AttachmentVideoNote, Label: "0:12"}, "[Video note: 0:12]"},
		{"quiz", &Attachment{Kind: AttachmentQuiz, Label: `"2+2?"`}, `[Quiz: "2+2?"]`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := tt.attachment.Placeholder(); got != tt.want {
				t.Fatalf("Placeholder() = %q, want %q", got, tt.want)
			}
		})
	}
}

// Every declared kind must have a readable display name.
func TestAttachmentPlaceholderCoversAllKinds(t *testing.T) {
	t.Parallel()

	kinds := []AttachmentKind{
		AttachmentSticker, AttachmentPoll, AttachmentQuiz, AttachmentDice, AttachmentChart,
		AttachmentAnimation, AttachmentVideo, AttachmentVideoNote, AttachmentVoice,
		AttachmentAudio, AttachmentDocument, AttachmentLocation, AttachmentContact,
	}
	for _, kind := range kinds {
		got := (&Attachment{Kind: kind, Label: "x"}).Placeholder()
		if got == "" || got[0] != '[' || got[len(got)-1] != ']' {
			t.Errorf("kind %q renders %q", kind, got)
		}
		if got[1] < 'A' || got[1] > 'Z' {
			t.Errorf("kind %q display name must be capitalized, got %q", kind, got)
		}
	}
}
