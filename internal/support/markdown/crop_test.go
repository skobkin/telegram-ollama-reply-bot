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

// TestCropRichMarkdown_HeadingThenUnspacedRun is a review regression: a short
// heading or preface followed by a long unspaced CJK paragraph must keep a
// near-full prefix; the boundary search may not backtrack past the window and
// collapse the message back to the heading.
func TestCropRichMarkdown_HeadingThenUnspacedRun(t *testing.T) {
	const limit = 32768
	text := "# 概要\n" + strings.Repeat("漢", 40000)

	got, changed := CropRichMarkdown(text, limit)

	if !changed {
		t.Fatalf("expected crop to modify text")
	}
	if n := utf8.RuneCountInString(got); n != limit {
		t.Fatalf("expected %d runes, got %d", limit, n)
	}
	if !strings.HasPrefix(got, "# 概要\n") {
		t.Fatalf("expected heading preserved, got head %q", got[:20])
	}
	if !strings.HasSuffix(got, "漢…") {
		t.Fatalf("expected hard cut inside the paragraph, got tail %q", got[len(got)-10:])
	}
}

// TestCropRichMarkdown_HeadingThenUnspacedRunSmallLimit checks the same
// protection when the limit is smaller than the backtrack window, so the
// search floor is the start of the text rather than the window edge.
func TestCropRichMarkdown_HeadingThenUnspacedRunSmallLimit(t *testing.T) {
	const limit = 400
	text := "# 概要\n" + strings.Repeat("漢", 600)

	got, changed := CropRichMarkdown(text, limit)

	if !changed {
		t.Fatalf("expected crop to modify text")
	}
	if n := utf8.RuneCountInString(got); n != limit {
		t.Fatalf("expected %d runes, got %d", limit, n)
	}
	if !strings.HasPrefix(got, "# 概要\n") {
		t.Fatalf("expected heading preserved, got head %q", got[:20])
	}
	if !strings.HasSuffix(got, "漢…") {
		t.Fatalf("expected hard cut inside the paragraph, got tail %q", got[len(got)-10:])
	}
}

// TestCropRichMarkdown_FencedLongLine is a review regression: an oversized
// single line inside a code fence must hard-cut inside the line rather than
// backtracking to the fence opener and dropping the code content. The broken
// fence is acceptable: GFM renders unclosed fences until the end of the
// message.
func TestCropRichMarkdown_FencedLongLine(t *testing.T) {
	const limit = 32768
	text := "```go\n" + strings.Repeat("漢", 40000) + "\n```"

	got, changed := CropRichMarkdown(text, limit)

	if !changed {
		t.Fatalf("expected crop to modify text")
	}
	if n := utf8.RuneCountInString(got); n != limit {
		t.Fatalf("expected %d runes, got %d", limit, n)
	}
	if !strings.HasPrefix(got, "```go\n") {
		t.Fatalf("expected fence opener preserved, got head %q", got[:20])
	}
	if !strings.HasSuffix(got, "漢…") {
		t.Fatalf("expected hard cut inside the code line, got tail %q", got[len(got)-10:])
	}
}

// TestCropRichMarkdown_NearbyBoundaryStillWins ensures the window does not
// break ordinary behavior: a whitespace boundary that sits within the window
// of the budget still attracts the cut, rather than triggering a hard cut.
func TestCropRichMarkdown_NearbyBoundaryStillWins(t *testing.T) {
	const limit = 32768
	// The only whitespace in the overshooting tail sits 300 runes before
	// the budget — within the backtrack window.
	boundary := limit - 1 - 300
	text := strings.Repeat("漢", boundary) + "\n" + strings.Repeat("x", 800)

	got, changed := CropRichMarkdown(text, limit)

	if !changed {
		t.Fatalf("expected crop to modify text")
	}
	if n := utf8.RuneCountInString(got); n != boundary+1 {
		t.Fatalf("expected cut at the in-window boundary (%d runes), got %d", boundary+1, n)
	}
	if !strings.HasSuffix(got, "漢…") {
		t.Fatalf("expected cut at the boundary, got tail %q", got[len(got)-10:])
	}
}
