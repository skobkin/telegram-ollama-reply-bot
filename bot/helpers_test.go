package bot

import (
	"strings"
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

func TestSanitizeAndCrop_LongReply(t *testing.T) {
	s := markdown.NewTgMarkdownV2Sanitizer()
	long := strings.Repeat("a", 5000) + "*"
	sanitized := s.Sanitize(long)
	cropped := cropToMaxLengthMarkdownV2(sanitized, 100)
	cropped = s.Sanitize(cropped)
	if len([]rune(cropped)) > 100 {
		t.Fatalf("cropped text length %d exceeds limit", len([]rune(cropped)))
	}
	if strings.Contains(cropped, "*") {
		t.Fatalf("cropped text contains unescaped marker: %q", cropped)
	}
}
