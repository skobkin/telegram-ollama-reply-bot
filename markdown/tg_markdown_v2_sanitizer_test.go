package markdown

import "testing"

func TestTgMarkdownV2Sanitizer_PreservesFormatting(t *testing.T) {
	s := NewTgMarkdownV2Sanitizer()
	input := "*bold* _italic_ __underline__ ~strike~ ||spoiler|| `code` ```\npre\n``` > quote [link](https://example.com/path_(1))"
	expected := "*bold* _italic_ __underline__ ~strike~ ||spoiler|| `code` ```\npre\n``` > quote [link](https://example.com/path_\\(1\\))"
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

func TestTgMarkdownV2Sanitizer_EscapeURL(t *testing.T) {
	s := NewTgMarkdownV2Sanitizer()
	input := "https://example.com/a(b)\\c"
	expected := "https://example.com/a\\(b\\)\\\\c"
	got := s.EscapeURL(input)
	if got != expected {
		t.Fatalf("unexpected escaped url: expected %q got %q", expected, got)
	}
}
