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

// CropRichMarkdown crops rich markdown text to at most limit runes, appending
// a plain ellipsis. The cut backtracks to the last whitespace boundary
// (including newlines and Unicode whitespace); when the prefix contains no
// whitespace at all, it cuts at the limit so the message keeps a useful
// prefix instead of collapsing to the ellipsis alone. Rich Markdown tolerates
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
	for cropPoint > 0 && !unicode.IsSpace(runes[cropPoint]) {
		cropPoint--
	}
	for cropPoint > 0 && unicode.IsSpace(runes[cropPoint-1]) {
		cropPoint--
	}
	if cropPoint == 0 {
		cropPoint = limit - 1
	}

	return string(runes[:cropPoint]) + "…", true
}
