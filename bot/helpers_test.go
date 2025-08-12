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
	cropped, changed := cropToMaxLengthMarkdownV2(sanitized, 12)
	if !changed {
		t.Fatalf("expected crop to modify text")
	}
	expected := "\\*bold\\.\\.\\."
	if cropped != expected {
		t.Fatalf("unexpected cropped text: expected %q got %q", expected, cropped)
	}
}

func TestSanitizeAndCrop_LongReply(t *testing.T) {
	s := markdown.NewTgMarkdownV2Sanitizer()
	long := strings.Repeat("a", 5000) + "*"
	sanitized := s.Sanitize(long)
	cropped, changed := cropToMaxLengthMarkdownV2(sanitized, 100)
	if changed {
		cropped = s.Sanitize(cropped)
	}
	if len([]rune(cropped)) > 100 {
		t.Fatalf("cropped text length %d exceeds limit", len([]rune(cropped)))
	}
	if strings.Contains(cropped, "*") {
		t.Fatalf("cropped text contains unescaped marker: %q", cropped)
	}
}

func TestSanitizeAndCrop_UnclosedCode(t *testing.T) {
	s := markdown.NewTgMarkdownV2Sanitizer()
	const repeat = 20
	code := strings.Repeat("abcd ", repeat)
	text := "`" + code + "` trailing"
	sanitized := s.Sanitize(text)
	closing := "` trailing"
	limit := len([]rune(sanitized)) - len([]rune(closing))
	cropped, changed := cropToMaxLengthMarkdownV2(sanitized, limit)
	if !changed {
		t.Fatalf("expected crop to modify text")
	}
	cropped = s.Sanitize(cropped)
	if len([]rune(cropped)) > limit {
		t.Fatalf("cropped text length %d exceeds limit", len([]rune(cropped)))
	}
	runes := []rune(cropped)
	for i, r := range runes {
		if r == '`' {
			if i == 0 || runes[i-1] != '\\' {
				t.Fatalf("cropped text contains unescaped backtick: %q", cropped)
			}
		}
	}
}

func TestSanitizeAndCrop_UnclosedFencedCode(t *testing.T) {
	s := markdown.NewTgMarkdownV2Sanitizer()
	const repeat = 20
	code := strings.Repeat("line\n", repeat)
	text := "```\n" + code + "``` trailing"
	sanitized := s.Sanitize(text)
	closing := "``` trailing"
	limit := len([]rune(sanitized)) - len([]rune(closing))
	cropped, changed := cropToMaxLengthMarkdownV2(sanitized, limit)
	if !changed {
		t.Fatalf("expected crop to modify text")
	}
	cropped = s.Sanitize(cropped)
	if len([]rune(cropped)) > limit {
		t.Fatalf("cropped text length %d exceeds limit", len([]rune(cropped)))
	}
	runes := []rune(cropped)
	for i := 0; i < len(runes); i++ {
		if runes[i] == '`' {
			if i > 0 && runes[i-1] == '\\' {
				continue
			}
			if i+2 < len(runes) && runes[i+1] == '`' && runes[i+2] == '`' {
				t.Fatalf("cropped text contains unclosed fenced marker: %q", cropped)
			}
			t.Fatalf("cropped text contains unescaped backtick: %q", cropped)
		}
	}
}
