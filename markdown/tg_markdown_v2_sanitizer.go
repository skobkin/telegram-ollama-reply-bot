package markdown

import "strings"

// Sanitizer escapes strings according to Telegram Markdown V2 rules while
// preserving entities supported by Telegram such as bold, italic, underline,
// strikethrough, spoiler, inline and fenced code blocks, block quotes, inline
// links, user mentions and custom emojis. It targets Telegram Markdown V2 only
// and is not suitable for generic Markdown content.
type Sanitizer interface {
	Sanitize(text string) string
	EscapeURL(url string) string
}

// NewTgMarkdownV2Sanitizer returns a Sanitizer for Telegram Markdown V2.
func NewTgMarkdownV2Sanitizer() Sanitizer {
	return tgMarkdownV2Sanitizer{}
}

type tgMarkdownV2Sanitizer struct{}

// Sanitize escapes characters in text according to Telegram Markdown V2 rules
// while keeping supported formatting entities intact.
func (s tgMarkdownV2Sanitizer) Sanitize(text string) string {
	var b strings.Builder
	runes := []rune(text)
	var boldOpen, italicOpen, underlineOpen, strikeOpen, spoilerOpen bool
	for i := 0; i < len(runes); i++ {
		r := runes[i]

		// handle code blocks
		if r == '`' {
			// fenced code block
			if i+2 < len(runes) && runes[i+1] == '`' && runes[i+2] == '`' {
				end := i + 3
				for end+2 < len(runes) {
					if runes[end] == '`' && runes[end+1] == '`' && runes[end+2] == '`' {
						break
					}
					end++
				}
				if end+2 < len(runes) {
					b.WriteString("```")
					i += 3
					for i < end {
						if runes[i] == '\\' || runes[i] == '`' {
							b.WriteRune('\\')
						}
						b.WriteRune(runes[i])
						i++
					}
					b.WriteString("```")
					i = end + 2
				} else {
					b.WriteString("\\`\\`\\`")
					i += 2
				}
				continue
			}

			// inline code
			end := i + 1
			for end < len(runes) && runes[end] != '`' {
				end++
			}
			if end < len(runes) {
				b.WriteRune('`')
				i++
				for i < end {
					if runes[i] == '\\' || runes[i] == '`' {
						b.WriteRune('\\')
					}
					b.WriteRune(runes[i])
					i++
				}
				b.WriteRune('`')
				i = end
			} else {
				b.WriteString("\\`")
			}
			continue
		}

		// handle simple formatting entities
		if r == '*' {
			if boldOpen {
				b.WriteRune('*')
				boldOpen = false
				continue
			}
			end := i + 1
			for end < len(runes) {
				if runes[end] == '\\' {
					end += 2
					continue
				}
				if runes[end] == '*' {
					break
				}
				end++
			}
			if end < len(runes) {
				b.WriteRune('*')
				boldOpen = true
			} else {
				b.WriteString("\\*")
			}
			continue
		}
		if r == '_' {
			if i+1 < len(runes) && runes[i+1] == '_' {
				if underlineOpen {
					b.WriteString("__")
					underlineOpen = false
					i++
					continue
				}
				end := i + 2
				for end+1 < len(runes) {
					if runes[end] == '\\' {
						end += 2
						continue
					}
					if runes[end] == '_' && runes[end+1] == '_' {
						break
					}
					end++
				}
				if end+1 < len(runes) {
					b.WriteString("__")
					underlineOpen = true
					i++
				} else {
					b.WriteString("\\_\\_")
					i++
				}
				continue
			}
			if italicOpen {
				b.WriteRune('_')
				italicOpen = false
				continue
			}
			end := i + 1
			for end < len(runes) {
				if runes[end] == '\\' {
					end += 2
					continue
				}
				if runes[end] == '_' {
					break
				}
				end++
			}
			if end < len(runes) {
				b.WriteRune('_')
				italicOpen = true
			} else {
				b.WriteString("\\_")
			}
			continue
		}
		if r == '~' {
			if strikeOpen {
				b.WriteRune('~')
				strikeOpen = false
				continue
			}
			end := i + 1
			for end < len(runes) {
				if runes[end] == '\\' {
					end += 2
					continue
				}
				if runes[end] == '~' {
					break
				}
				end++
			}
			if end < len(runes) {
				b.WriteRune('~')
				strikeOpen = true
			} else {
				b.WriteString("\\~")
			}
			continue
		}
		if r == '|' {
			if i+1 < len(runes) && runes[i+1] == '|' {
				if spoilerOpen {
					b.WriteString("||")
					spoilerOpen = false
					i++
					continue
				}
				end := i + 2
				for end+1 < len(runes) {
					if runes[end] == '\\' {
						end += 2
						continue
					}
					if runes[end] == '|' && runes[end+1] == '|' {
						break
					}
					end++
				}
				if end+1 < len(runes) {
					b.WriteString("||")
					spoilerOpen = true
					i++
				} else {
					b.WriteString("\\|\\|")
					i++
				}
			} else {
				b.WriteString("\\|")
			}
			continue
		}

		// links and user mentions/custom emoji
		if r == '[' {
			end := i + 1
			for end < len(runes) && runes[end] != ']' {
				end++
			}
			if end < len(runes)-1 && runes[end+1] == '(' {
				// link-like structure
				textPart := s.Sanitize(string(runes[i+1 : end]))
				b.WriteRune('[')
				b.WriteString(textPart)
				b.WriteString("](")
				urlStart := end + 2
				urlEnd := urlStart
				depth := 1
				for urlEnd < len(runes) {
					if runes[urlEnd] == '(' {
						depth++
					} else if runes[urlEnd] == ')' {
						depth--
						if depth == 0 {
							break
						}
					}
					urlEnd++
				}
				urlPart := s.EscapeURL(string(runes[urlStart:urlEnd]))
				b.WriteString(urlPart)
				b.WriteRune(')')
				i = urlEnd
				continue
			}
			// not a link, escape
			b.WriteString("\\[")
			continue
		}
		if r == ']' {
			b.WriteString("\\]")
			continue
		}
		if r == '(' {
			b.WriteString("\\(")
			continue
		}
		if r == ')' {
			b.WriteString("\\)")
			continue
		}
		if r == '\\' {
			if i+1 < len(runes) {
				next := runes[i+1]
				if strings.ContainsRune("_*[]()~`># +- =|{}.!", next) {
					b.WriteRune('\\')
					b.WriteRune(next)
					i++
					continue
				}
			}
			b.WriteString("\\\\")
			continue
		}
		if r == '!' {
			if i+1 < len(runes) && runes[i+1] == '[' {
				end := i + 2
				for end < len(runes) && runes[end] != ']' {
					end++
				}
				if end < len(runes)-1 && runes[end+1] == '(' {
					urlStart := end + 2
					urlEnd := urlStart
					depth := 1
					for urlEnd < len(runes) {
						if runes[urlEnd] == '(' {
							depth++
						} else if runes[urlEnd] == ')' {
							depth--
							if depth == 0 {
								break
							}
						}
						urlEnd++
					}
					rawURL := string(runes[urlStart:urlEnd])
					escapedURL := s.EscapeURL(rawURL)
					if strings.HasPrefix(rawURL, "tg://") {
						alt := s.Sanitize(string(runes[i+2 : end]))
						b.WriteString("![")
						b.WriteString(alt)
						b.WriteString("](")
						b.WriteString(escapedURL)
						b.WriteRune(')')
					} else {
						b.WriteString(escapedURL)
					}
					i = urlEnd
					continue
				}
			}
			b.WriteString("\\!")
			continue
		}
		if r == '>' {
			j := i - 1
			for j >= 0 && runes[j] != '\n' {
				if runes[j] == '*' || runes[j] == '_' || runes[j] == '~' || runes[j] == '|' {
					j--
					continue
				}
				if runes[j] == '\\' {
					j -= 2
					continue
				}
				break
			}
			if j < 0 || runes[j] == '\n' {
				b.WriteRune('>')
			} else {
				b.WriteString("\\>")
			}
			continue
		}
		switch r {
		case '#', '+', '-', '=', '{', '}', '.':
			b.WriteRune('\\')
			b.WriteRune(r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// EscapeURL escapes URL characters required by Telegram Markdown V2 inside link URLs.
func (s tgMarkdownV2Sanitizer) EscapeURL(url string) string {
	var b strings.Builder
	for _, r := range url {
		if r == '(' || r == ')' || r == '\\' {
			b.WriteRune('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}
