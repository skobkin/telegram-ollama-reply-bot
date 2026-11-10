package markdown

import (
	"regexp"
	"strings"
)

// richImageRefPattern matches image references of the form ![alt](destination)
// or ![alt](destination "title") as accepted by GitHub Flavored Markdown.
var richImageRefPattern = regexp.MustCompile(`!\[[^\]]*\]\(([^)]*)\)`)

// fenceMarkerPattern matches the opening or closing marker of a fenced code
// block at the start of a line: three or more backticks (\x60) or tildes,
// optionally indented by up to three spaces (GitHub Flavored Markdown).
var fenceMarkerPattern = regexp.MustCompile(`^ {0,3}(\x60{3,}|~{3,})`)

// SanitizeRichMarkdown normalizes raw LLM markdown for Telegram Rich Messages.
//
// Rich Markdown is GitHub Flavored Markdown and tolerates unbalanced markup
// (unclosed emphasis renders as literal text), so no escaping or marker
// re-balancing is performed — only two documented Telegram constraints are
// normalized:
//
//  1. Media blocks support only HTTP and HTTPS URLs: image references whose
//     destination is empty or not http(s) are replaced with the destination as
//     plain text (references with an empty destination are dropped).
//  2. An unclosed fenced code block is closed by appending a matching fence,
//     so the message tail is not swallowed into a code block. Cropping a
//     message can create such a fence.
//
// Everything else — headings, tables, task lists, footnotes, HTML — passes
// through untouched, preserving the model's intended formatting. Structural
// limits that cannot be checked cheaply on a character level (block count,
// nesting depth, media count, table columns) are left to the API; the caller
// is expected to fall back when the send fails.
func SanitizeRichMarkdown(text string) string {
	lines := strings.Split(text, "\n")

	insideFence := false
	fenceChar := byte('`')

	for i, line := range lines {
		if insideFence {
			if isFenceClose(line, fenceChar) {
				insideFence = false
			}

			// Fenced code content is passed through untouched.
			continue
		}

		if marker := fenceMarker(line); marker != 0 {
			insideFence = true
			fenceChar = marker

			continue
		}

		lines[i] = replaceNonHTTPImageRefs(line)
	}

	if !insideFence {
		return strings.Join(lines, "\n")
	}

	// Close the dangling fence so the rest of the message renders as normal
	// markdown instead of an endless code block.
	closer := "```"
	if fenceChar == '~' {
		closer = "~~~"
	}

	joined := strings.Join(lines, "\n")
	if strings.HasSuffix(joined, "\n") {
		return joined + closer
	}

	return joined + "\n" + closer
}

// replaceNonHTTPImageRefs rewrites image references that Telegram cannot
// render as media blocks into their destination URL as plain text.
func replaceNonHTTPImageRefs(line string) string {
	return richImageRefPattern.ReplaceAllStringFunc(line, func(ref string) string {
		sub := richImageRefPattern.FindStringSubmatch(ref)
		if len(sub) < 2 {
			return ref
		}

		destination := sub[1]
		if i := strings.IndexAny(destination, " \t"); i >= 0 {
			destination = destination[:i] // strip an optional "title"
		}
		destination = strings.TrimSpace(destination)
		if strings.HasPrefix(destination, "\"") || strings.HasPrefix(destination, "'") {
			// The parenthesized content is only a title: no destination.
			destination = ""
		} else {
			destination = strings.Trim(destination, "<>")
		}

		lower := strings.ToLower(destination)
		if strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://") {
			return ref
		}

		return destination
	})
}

// fenceMarker returns the fence character (` or ~) when the line opens a
// fenced code block, or 0 otherwise.
func fenceMarker(line string) byte {
	sub := fenceMarkerPattern.FindStringSubmatch(line)
	if len(sub) < 2 {
		return 0
	}

	return sub[1][0]
}

// isFenceClose reports whether the line closes a fence opened with the given
// character. A closing fence uses the same character and at least three of it.
func isFenceClose(line string, fenceChar byte) bool {
	sub := fenceMarkerPattern.FindStringSubmatch(line)
	if len(sub) < 2 {
		return false
	}

	return sub[1][0] == fenceChar
}
