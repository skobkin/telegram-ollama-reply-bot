package markdown

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
// a plain ellipsis at the last whitespace boundary. Rich Markdown tolerates
// unbalanced markup, so no marker re-balancing is performed; a code fence
// broken by the crop is closed later by SanitizeRichMarkdown, which must run
// after this function. It returns the cropped text and whether it changed.
func CropRichMarkdown(text string, limit int) (string, bool) {
	if limit <= 0 {
		return text, false
	}

	runes := []rune(text)
	if len(runes) <= limit {
		return text, false
	}

	const ellipsis = "…"
	const ellipsisRunes = 1

	cropPoint := limit - ellipsisRunes
	if cropPoint > len(runes) {
		cropPoint = len(runes)
	}
	for cropPoint > 0 && runes[cropPoint] != ' ' {
		cropPoint--
	}

	return string(runes[:cropPoint]) + ellipsis, true
}
