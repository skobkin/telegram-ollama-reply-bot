package markdown

import (
	"strings"
	"testing"
)

func TestSanitizeRichMarkdown_KeepsHTTPImageRefs(t *testing.T) {
	text := "look:\n![chart](https://example.com/chart.png)\nand ![pic](http://example.org/p.png \"title\")"
	if got := SanitizeRichMarkdown(text); got != text {
		t.Fatalf("expected text unchanged, got %q", got)
	}
}

func TestSanitizeRichMarkdown_ReplacesNonHTTPImageRefsWithURL(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"relative path", "see ![icon](file.png) here", "see file.png here"},
		{"data uri", "![blob](data:image/png;base64,AAAA)", "data:image/png;base64,AAAA"},
		{"magnet link", "![x](magnet:?xt=urn:btih:123)", "magnet:?xt=urn:btih:123"},
		{"empty destination", "before ![]() after", "before  after"},
		{"empty with title", "before ![](\"title\") after", "before  after"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := SanitizeRichMarkdown(tc.in); got != tc.want {
				t.Fatalf("expected %q, got %q", tc.want, got)
			}
		})
	}
}

func TestSanitizeRichMarkdown_UntouchedInsideFence(t *testing.T) {
	text := "intro\n```markdown\n![icon](file.png)\n```\nrest"
	if got := SanitizeRichMarkdown(text); got != text {
		t.Fatalf("expected fenced content untouched, got %q", got)
	}
}

func TestSanitizeRichMarkdown_KeepsBalancedFences(t *testing.T) {
	text := "```go\nfmt.Println(1)\n```\ndone"
	if got := SanitizeRichMarkdown(text); got != text {
		t.Fatalf("expected balanced fences unchanged, got %q", got)
	}
}

func TestSanitizeRichMarkdown_ClosesUnclosedFence(t *testing.T) {
	text := "before\n```go\ncode without end"
	want := text + "\n```"
	if got := SanitizeRichMarkdown(text); got != want {
		t.Fatalf("expected %q, got %q", want, got)
	}
}

func TestSanitizeRichMarkdown_ClosesUnclosedTildeFence(t *testing.T) {
	text := "~~~\nplain"
	want := text + "\n~~~"
	if got := SanitizeRichMarkdown(text); got != want {
		t.Fatalf("expected %q, got %q", want, got)
	}
}

func TestSanitizeRichMarkdown_NoCloserForEmptyTail(t *testing.T) {
	// A fence closed on the very last line must not gain an extra closer.
	text := "open\n```\ncode\n```"
	if got := SanitizeRichMarkdown(text); got != text {
		t.Fatalf("expected %q, got %q", text, got)
	}
}

func TestSanitizeRichMarkdown_LeavesEverythingElseRaw(t *testing.T) {
	text := "# Heading\n| a | b |\n|---|:-:|\n| 1 | 2 |\n**bold** ~~strike~~ ==mark== ||spoiler|| <u>u</u> * not a list"
	if got := SanitizeRichMarkdown(text); got != text {
		t.Fatalf("expected raw passthrough, got %q", got)
	}
}

func TestCropRichMarkdown_UnderLimitIsNoOp(t *testing.T) {
	text := "short text"
	got, changed := CropRichMarkdown(text, 100)
	if changed || got != text {
		t.Fatalf("expected no-op, got %q changed=%v", got, changed)
	}
}

func TestCropRichMarkdown_CropsAtWhitespaceWithEllipsis(t *testing.T) {
	text := strings.Repeat("word ", 100) // 500 runes
	got, changed := CropRichMarkdown(text, 50)
	if !changed {
		t.Fatalf("expected crop to modify text")
	}
	runes := []rune(got)
	if len(runes) > 50 {
		t.Fatalf("cropped length %d exceeds limit", len(runes))
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("expected ellipsis suffix, got %q", got)
	}
	if strings.HasSuffix(strings.TrimSuffix(got, "…"), " ") {
		t.Fatalf("expected crop to end at a word boundary, got %q", got)
	}
}

func TestCropRichMarkdown_MultibyteSafe(t *testing.T) {
	text := strings.Repeat("тест ", 1000) // Cyrillic, 2 bytes per rune
	got, changed := CropRichMarkdown(text, 51)
	if !changed {
		t.Fatalf("expected crop to modify text")
	}
	runes := []rune(got)
	if len(runes) > 51 {
		t.Fatalf("cropped rune length %d exceeds limit", len(runes))
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("expected ellipsis suffix, got %q", got)
	}
}

func TestCropThenSanitize_ClosesFenceBrokenByCrop(t *testing.T) {
	// A long fenced block cropped in the middle leaves an unclosed fence;
	// the subsequent sanitize pass must close it.
	code := strings.Repeat("line of code\n", 5000)
	text := "```go\n" + code + "```"

	cropped, changed := CropRichMarkdown(text, 10000)
	if !changed {
		t.Fatalf("expected crop to modify text")
	}
	if strings.Contains(cropped, "\n```") {
		t.Fatalf("expected crop to break the closing fence, got intact fences")
	}

	sanitized := SanitizeRichMarkdown(cropped)
	if !strings.HasSuffix(sanitized, "\n```") {
		t.Fatalf("expected sanitize to append a closing fence, got %q", tail(sanitized, 20))
	}
}

func tail(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}

	return string(runes[len(runes)-n:])
}
