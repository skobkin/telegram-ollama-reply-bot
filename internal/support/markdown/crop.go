package markdown

import "unicode"

// CropToMaxLengthMarkdownV2 crops sanitized Telegram Markdown V2 text to at
// most limit runes, appending an escaped ellipsis. It backtracks to an
// unescaped whitespace boundary and re-escapes any formatting markers the
// crop left unbalanced. It returns the cropped text and whether it changed.
func CropToMaxLengthMarkdownV2(text string, limit int) (string, bool) {
	runes := []rune(text)
	if len(runes) <= limit {
		return text, false
	}

	const ellipsis = "\\.\\.\\."
	ellipsisLen := len(ellipsis)

	cropPoint := limit - ellipsisLen
	if cropPoint > len(runes) {
		cropPoint = len(runes)
	}
	for cropPoint > 0 && (runes[cropPoint] != ' ' || runes[cropPoint-1] == '\\') {
		cropPoint--
	}

	croppedRunes := runes[:cropPoint]

	// escape dangling formatting markers
	markers := []rune{'*', '_', '~', '|', '`'}
	for _, m := range markers {
		unescapedCount := 0
		lastIdx := -1
		for i := 0; i < len(croppedRunes); i++ {
			if croppedRunes[i] == '\\' {
				i++

				continue
			}
			if croppedRunes[i] == m {
				unescapedCount++
				lastIdx = i
			}
		}
		if unescapedCount%2 != 0 && lastIdx >= 0 {
			croppedRunes = append(croppedRunes[:lastIdx], append([]rune{'\\'}, croppedRunes[lastIdx:]...)...)
		}
	}

	if len(croppedRunes)+ellipsisLen > limit {
		cropPoint := limit - ellipsisLen
		if cropPoint > len(croppedRunes) {
			cropPoint = len(croppedRunes)
		}
		for cropPoint > 0 && croppedRunes[cropPoint-1] == '\\' {
			cropPoint--
		}
		croppedRunes = croppedRunes[:cropPoint]
	}

	return string(croppedRunes) + ellipsis, true
}

// cropBacktrackWindow bounds how far CropRichMarkdown searches backwards
// from the desired cut for a whitespace boundary (and never further than
// half the budget). Without it, a short heading or preface followed by a
// long unspaced run (e.g. a CJK paragraph or an oversized code line) would
// drag the cut point all the way back to the preface and discard the
// paragraph almost entirely.
const cropBacktrackWindow = 512

// CropRichMarkdown crops rich markdown text to at most limit runes, appending
// a plain ellipsis. The cut backtracks to the nearest whitespace boundary
// (including newlines and Unicode whitespace) within cropBacktrackWindow
// runes of the budget; when no boundary exists that close, it hard-cuts at
// the limit so the message keeps a near-full prefix instead of collapsing
// to its short preface or the ellipsis alone. Rich Markdown tolerates
// unbalanced markup, so no marker re-balancing is performed: GitHub Flavored
// Markdown renders an unclosed construct, such as a code fence broken by the
// crop, gracefully until the end of the message.
func CropRichMarkdown(text string, limit int) (string, bool) {
	if limit <= 0 {
		return text, false
	}

	runes := []rune(text)
	if len(runes) <= limit {
		return text, false
	}

	cropPoint := limit - 1 // reserve one rune for the ellipsis

	// The search never crosses lowerBound: backing further up risks losing
	// almost the whole message to a distant boundary. The window is also
	// capped at half the budget so small limits keep a proportional prefix
	// instead of collapsing to whatever boundary sits near the start.
	window := limit / 2
	if window > cropBacktrackWindow {
		window = cropBacktrackWindow
	}
	lowerBound := limit - 1 - window
	if lowerBound < 1 {
		lowerBound = 1
	}

	for cropPoint > lowerBound && !unicode.IsSpace(runes[cropPoint]) {
		cropPoint--
	}

	if unicode.IsSpace(runes[cropPoint]) {
		// Trim the whole whitespace run so the ellipsis sits tight after
		// the last word.
		for cropPoint > lowerBound && unicode.IsSpace(runes[cropPoint-1]) {
			cropPoint--
		}
	} else {
		// No boundary near the cut: hard-cut at the rune budget.
		cropPoint = limit - 1
	}

	return string(runes[:cropPoint]) + "…", true
}
