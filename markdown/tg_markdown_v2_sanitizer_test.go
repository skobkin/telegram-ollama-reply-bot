package markdown

import (
	"strings"
	"testing"
)

func TestTgMarkdownV2Sanitizer_PreservesFormatting(t *testing.T) {
	s := NewTgMarkdownV2Sanitizer()
	input := "*bold* _italic_ __underline__ ~strike~ ||spoiler|| `code` ```\npre\n``` > quote [link](https://example.com/path_(1))"
	expected := "*bold* _italic_ __underline__ ~strike~ ||spoiler|| `code` ```\npre\n``` \\> quote [link](https://example.com/path_\\(1\\))"
	got := s.Sanitize(input)
	if got != expected {
		t.Fatalf("unexpected sanitized text:\nexpected: %q\nactual:   %q", expected, got)
	}
}

func TestTgMarkdownV2Sanitizer_EscapesIllegalChars(t *testing.T) {
	s := NewTgMarkdownV2Sanitizer()
	input := "Hello [World]! (test). `code \\`"
	expected := "Hello \\[World\\]\\! \\(test\\)\\. `code \\\\`"
	got := s.Sanitize(input)
	if got != expected {
		t.Fatalf("unexpected sanitized text:\nexpected: %q\nactual:   %q", expected, got)
	}
}

func TestTgMarkdownV2Sanitizer_UnclosedInlineCode(t *testing.T) {
	s := NewTgMarkdownV2Sanitizer()
	input := "Here is `code"
	expected := "Here is \\`code"
	if got := s.Sanitize(input); got != expected {
		t.Fatalf("unexpected sanitized text:\nexpected: %q\nactual:   %q", expected, got)
	}
}

func TestTgMarkdownV2Sanitizer_UnclosedFencedCode(t *testing.T) {
	s := NewTgMarkdownV2Sanitizer()
	input := "```\nline"
	expected := "\\`\\`\\`\nline"
	if got := s.Sanitize(input); got != expected {
		t.Fatalf("unexpected sanitized text:\nexpected: %q\nactual:   %q", expected, got)
	}
}

func TestTgMarkdownV2Sanitizer_UnclosedFormatting(t *testing.T) {
	s := NewTgMarkdownV2Sanitizer()
	cases := []struct{ in, out string }{
		{"Here is *bold", "Here is \\*bold"},
		{"Here is _italic", "Here is \\_italic"},
		{"Here is __underline", "Here is \\_\\_underline"},
		{"Here is ~strike", "Here is \\~strike"},
		{"Here is ||spoiler", "Here is \\|\\|spoiler"},
	}
	for _, tc := range cases {
		if got := s.Sanitize(tc.in); got != tc.out {
			t.Fatalf("unexpected sanitized text for %q:\nexpected: %q\nactual:   %q", tc.in, tc.out, got)
		}
	}
}

func TestTgMarkdownV2Sanitizer_OfficialExamples(t *testing.T) {
	s := NewTgMarkdownV2Sanitizer()
	lines := []string{
		"*bold \\*text*",
		"_italic \\*text_",
		"__underline__",
		"~strikethrough~",
		"||spoiler||",
		"*bold _italic bold ~italic bold strikethrough ||italic bold strikethrough spoiler||~ __underline italic bold___ bold*",
		"[inline URL](http://www.example.com/)",
		"[inline mention of a user](tg://user?id=123456789)",
		"![👍](tg://emoji?id=5368324170671202286)",
		"`inline fixed-width code`",
		"```",
		"pre-formatted fixed-width code block",
		"```",
		"```python",
		"pre-formatted fixed-width code block written in the Python programming language",
		"```",
		">Block quotation started",
		">Block quotation continued",
		">Block quotation continued",
		">Block quotation continued",
		">The last line of the block quotation",
		"**>The expandable block quotation started right after the previous block quotation",
		">It is separated from the previous block quotation by an empty bold entity",
		">Expandable block quotation continued",
		">Hidden by default part of the expandable block quotation started",
		">Expandable block quotation continued",
		">The last line of the expandable block quotation with the expandability mark\\|\\|",
	}
	input := strings.Join(lines, "\n")
	if got := s.Sanitize(input); got != input {
		t.Fatalf("official example changed:\nexpected:\n%q\nactual:\n%q", input, got)
	}
}

func TestTgMarkdownV2Sanitizer_DisallowedTags(t *testing.T) {
	s := NewTgMarkdownV2Sanitizer()
	input := "<b>test</b> and > quote"
	expected := "<b\\>test</b\\> and \\> quote"
	if got := s.Sanitize(input); got != expected {
		t.Fatalf("unexpected sanitized text:\nexpected: %q\nactual:   %q", expected, got)
	}
}

func TestTgMarkdownV2Sanitizer_CustomEmoji(t *testing.T) {
	s := NewTgMarkdownV2Sanitizer()
	input := "![👍](tg://emoji?id=1)"
	if got := s.Sanitize(input); got != input {
		t.Fatalf("custom emoji altered: expected %q got %q", input, got)
	}
}

func TestTgMarkdownV2Sanitizer_EscapeURL(t *testing.T) {
	s := NewTgMarkdownV2Sanitizer()
	input := "https://example.com/a(b)\\c"
	expected := "https://example.com/a\\(b\\)\\\\c"
	got := s.EscapeURL(input)
	if got != expected {
		t.Fatalf("unexpected escaped url: expected %q got %q", expected, got)
	}
}
