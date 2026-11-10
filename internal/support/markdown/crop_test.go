package markdown

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestCropToMaxLengthMarkdownV2_SanitizesAfterCrop(t *testing.T) {
	text := "*bold text* trailing"
	s := NewTgMarkdownV2Sanitizer()
	sanitized := s.Sanitize(text)
	cropped, changed := CropToMaxLengthMarkdownV2(sanitized, 12)
	if !changed {
		t.Fatalf("expected crop to modify text")
	}
	expected := "\\*bold\\.\\.\\."
	if cropped != expected {
		t.Fatalf("unexpected cropped text: expected %q got %q", expected, cropped)
	}
}

func TestSanitizeAndCrop_LongReply(t *testing.T) {
	s := NewTgMarkdownV2Sanitizer()
	long := strings.Repeat("a", 5000) + "*"
	sanitized := s.Sanitize(long)
	cropped, changed := CropToMaxLengthMarkdownV2(sanitized, 100)
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
	s := NewTgMarkdownV2Sanitizer()
	const repeat = 20
	code := strings.Repeat("abcd ", repeat)
	text := "`" + code + "` trailing"
	sanitized := s.Sanitize(text)
	closing := "` trailing"
	limit := len([]rune(sanitized)) - len([]rune(closing))
	cropped, changed := CropToMaxLengthMarkdownV2(sanitized, limit)
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
	s := NewTgMarkdownV2Sanitizer()
	const repeat = 20
	code := strings.Repeat("line\n", repeat)
	text := "```\n" + code + "``` trailing"
	sanitized := s.Sanitize(text)
	closing := "``` trailing"
	limit := len([]rune(sanitized)) - len([]rune(closing))
	cropped, changed := CropToMaxLengthMarkdownV2(sanitized, limit)
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

func TestCropRichMarkdown_UnderLimitReturnsUnchanged(t *testing.T) {
	got, changed := CropRichMarkdown("short text", 100)
	if changed || got != "short text" {
		t.Fatalf("expected unchanged text, got changed=%v %q", changed, got)
	}
}

func TestCropRichMarkdown_AtWhitespaceBoundary(t *testing.T) {
	text := strings.Repeat("word ", 100)

	got, changed := CropRichMarkdown(text, 50)

	if !changed {
		t.Fatalf("expected crop to modify text")
	}
	if n := utf8.RuneCountInString(got); n > 50 {
		t.Fatalf("cropped text %d runes exceeds limit", n)
	}
	if !strings.HasSuffix(got, "word…") {
		t.Fatalf("expected crop at word boundary, got tail %q", got[len(got)-10:])
	}
}

func TestCropRichMarkdown_MultibyteRunesNeverSplit(t *testing.T) {
	text := strings.Repeat("тест ", 100)

	got, changed := CropRichMarkdown(text, 41)

	if !changed {
		t.Fatalf("expected crop to modify text")
	}
	if n := utf8.RuneCountInString(got); n > 41 {
		t.Fatalf("cropped text %d runes exceeds limit", n)
	}
	if !strings.HasSuffix(got, "тест…") {
		t.Fatalf("expected intact multibyte word before ellipsis, got tail %q", got[len(got)-10:])
	}
}

// TestCropRichMarkdown_NoWhitespaceKeepsPrefix is a review regression: text
// without any whitespace (e.g. CJK) must keep a full-length prefix instead of
// the crop backing up to zero and collapsing the message to the ellipsis.
func TestCropRichMarkdown_NoWhitespaceKeepsPrefix(t *testing.T) {
	const limit = 32768
	text := strings.Repeat("漢", limit+1)

	got, changed := CropRichMarkdown(text, limit)

	if !changed {
		t.Fatalf("expected crop to modify text")
	}
	if n := utf8.RuneCountInString(got); n != limit {
		t.Fatalf("expected %d runes, got %d", limit, n)
	}
	if want := strings.Repeat("漢", limit-1) + "…"; got != want {
		t.Fatalf("unexpected crop result: tail %q", got[len(got)-10:])
	}
}

// TestCropRichMarkdown_NewlineIsBoundary is a review regression: newlines and
// other Unicode whitespace must count as crop boundaries, not just ASCII
// spaces.
func TestCropRichMarkdown_NewlineIsBoundary(t *testing.T) {
	text := strings.Repeat("line\n", 100)

	got, changed := CropRichMarkdown(text, 51)

	if !changed {
		t.Fatalf("expected crop to modify text")
	}
	if n := utf8.RuneCountInString(got); n > 51 {
		t.Fatalf("cropped text %d runes exceeds limit", n)
	}
	if !strings.HasSuffix(got, "line…") {
		t.Fatalf("expected crop at newline boundary, got tail %q", got[len(got)-10:])
	}
}
