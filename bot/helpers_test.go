package bot

import (
	"testing"

	"telegram-ollama-reply-bot/markdown"
)

func TestCropToMaxLengthMarkdownV2_SanitizesAfterCrop(t *testing.T) {
	text := "*bold text* trailing"
	s := markdown.NewTgMarkdownV2Sanitizer()
	sanitized := s.Sanitize(text)
	cropped := cropToMaxLengthMarkdownV2(sanitized, 12)
	expected := "\\*bold\\.\\.\\."
	if cropped != expected {
		t.Fatalf("unexpected cropped text: expected %q got %q", expected, cropped)
	}
}
