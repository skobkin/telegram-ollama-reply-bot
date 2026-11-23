// Package msgdesc turns the non-text content of Telegram messages into the
// concise text descriptions retained in conversation history
// (state.Attachment). The same builders serve messages users send, messages
// the bot sends through its tools, and the fallback for quoted messages, so a
// sticker or poll reads identically wherever the model meets it.
//
// Descriptions are deliberately lossy and privacy-conservative: they carry
// what is needed to talk about a message ("the poll about dinner", "that
// 12-second voice note") and nothing that identifies a person or a place.
// Phone numbers, coordinates, file identifiers and binary content are never
// included.
package msgdesc

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	t "github.com/mymmrac/telego"

	"telegram-ollama-reply-bot/internal/state"
)

const (
	// maxLabelRunes bounds one whole label.
	maxLabelRunes = 160
	// maxPollQuestionRunes and maxPollOptionRunes keep a poll readable in a
	// single line; maxPollOptions caps how many options are listed.
	maxPollQuestionRunes = 80
	maxPollOptionRunes   = 32
	maxPollOptions       = 6
	maxFileNameRunes     = 48
	maxTitleRunes        = 48
	// maxEmojiRunes leaves room for multi-codepoint emoji sequences.
	maxEmojiRunes = 12
)

// Describe describes the non-text content of msg, or returns nil when it has
// none. Photos are intentionally not described: they keep the dedicated
// image representation (HasImage/ImageMeta) that the vision pipeline uses.
// Plain text, captions and other message kinds return nil.
func Describe(msg *t.Message) *state.Attachment {
	if msg == nil {
		return nil
	}

	// Order matters: an animation message also carries a Document, and a
	// video note must not be mistaken for a plain video.
	switch {
	case msg.Sticker != nil:
		return Sticker(msg.Sticker.Emoji, msg.Sticker.SetName)
	case msg.Poll != nil:
		options := make([]string, 0, len(msg.Poll.Options))
		for _, option := range msg.Poll.Options {
			options = append(options, option.Text)
		}

		return Poll(msg.Poll.Question, options, msg.Poll.Type == "quiz")
	case msg.Dice != nil:
		return Dice(msg.Dice.Emoji, msg.Dice.Value)
	case msg.Animation != nil:
		return &state.Attachment{Kind: state.AttachmentAnimation, Label: durationLabel(msg.Animation.Duration)}
	case msg.VideoNote != nil:
		return &state.Attachment{Kind: state.AttachmentVideoNote, Label: durationLabel(msg.VideoNote.Duration)}
	case msg.Video != nil:
		return &state.Attachment{Kind: state.AttachmentVideo, Label: durationLabel(msg.Video.Duration)}
	case msg.Voice != nil:
		return &state.Attachment{Kind: state.AttachmentVoice, Label: durationLabel(msg.Voice.Duration)}
	case msg.Audio != nil:
		return audio(msg.Audio)
	case msg.Document != nil:
		return &state.Attachment{Kind: state.AttachmentDocument, Label: truncate(clean(msg.Document.FileName), maxFileNameRunes)}
	case msg.Location != nil:
		// Coordinates identify a place; the fact of sharing one is enough.
		return &state.Attachment{Kind: state.AttachmentLocation}
	case msg.Contact != nil:
		// Phone numbers and names of third parties are never retained here.
		return &state.Attachment{Kind: state.AttachmentContact}
	default:
		return nil
	}
}

// Sticker describes a sticker by its emoji and either a human description
// (catalog stickers the bot sends) or its set name (stickers users send).
func Sticker(emoji, detail string) *state.Attachment {
	label := join(" ", truncate(clean(emoji), maxEmojiRunes), parenthesized(truncate(clean(detail), maxTitleRunes)))

	return &state.Attachment{Kind: state.AttachmentSticker, Label: truncate(label, maxLabelRunes)}
}

// Poll describes a poll or quiz by its question and options. A quiz's correct
// answer is never part of the description.
func Poll(question string, options []string, quiz bool) *state.Attachment {
	kind := state.AttachmentPoll
	if quiz {
		kind = state.AttachmentQuiz
	}

	shown := make([]string, 0, maxPollOptions)
	for _, option := range options {
		if len(shown) == maxPollOptions {
			break
		}
		if text := truncate(clean(option), maxPollOptionRunes); text != "" {
			shown = append(shown, text)
		}
	}

	label := `"` + truncate(clean(question), maxPollQuestionRunes) + `"`
	if len(shown) > 0 {
		label += " — " + strings.Join(shown, " / ")
		if extra := len(options) - len(shown); extra > 0 {
			label += fmt.Sprintf(" (+%d more)", extra)
		}
	}

	return &state.Attachment{Kind: kind, Label: truncate(label, maxLabelRunes)}
}

// Dice describes a dice-type animation and its outcome. A non-positive value
// means the outcome is unknown (Telegram's dice values start at 1) and is
// left out rather than reported as zero.
func Dice(emoji string, value int) *state.Attachment {
	outcome := ""
	if value > 0 {
		outcome = fmt.Sprintf("= %d", value)
	}

	return &state.Attachment{Kind: state.AttachmentDice, Label: join(" ", truncate(clean(emoji), maxEmojiRunes), outcome)}
}

// Chart describes a generated chart by its type and title.
func Chart(chartType, title string) *state.Attachment {
	label := clean(chartType)
	if title = truncate(clean(title), maxTitleRunes); title != "" {
		label = join(": ", label, title)
	}

	return &state.Attachment{Kind: state.AttachmentChart, Label: truncate(label, maxLabelRunes)}
}

func audio(a *t.Audio) *state.Attachment {
	title := truncate(clean(a.Title), maxTitleRunes)
	performer := truncate(clean(a.Performer), maxTitleRunes)

	label := title
	if performer != "" && title != "" {
		label = performer + " — " + title
	} else if performer != "" {
		label = performer
	}
	label = join(" ", label, parenthesized(durationLabel(a.Duration)))

	return &state.Attachment{Kind: state.AttachmentAudio, Label: truncate(label, maxLabelRunes)}
}

// durationLabel formats seconds as m:ss ("" for non-positive durations).
func durationLabel(seconds int) string {
	if seconds <= 0 {
		return ""
	}

	return fmt.Sprintf("%d:%02d", seconds/60, seconds%60)
}

func parenthesized(text string) string {
	if text == "" {
		return ""
	}

	return "(" + text + ")"
}

// join concatenates the non-empty parts with sep.
func join(sep string, parts ...string) string {
	kept := make([]string, 0, len(parts))
	for _, part := range parts {
		if part != "" {
			kept = append(kept, part)
		}
	}

	return strings.Join(kept, sep)
}

// clean makes untrusted text safe for a single-line label: control characters
// and line breaks become spaces, runs of whitespace collapse, ends are trimmed.
func clean(text string) string {
	var b strings.Builder
	b.Grow(len(text))

	space := false
	for _, r := range text {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			space = true

			continue
		}
		if space && b.Len() > 0 {
			b.WriteByte(' ')
		}
		space = false
		b.WriteRune(r)
	}

	return b.String()
}

// truncate shortens text to at most limit runes, marking the cut with "…".
func truncate(text string, limit int) string {
	if utf8.RuneCountInString(text) <= limit {
		return text
	}

	return string([]rune(text)[:limit-1]) + "…"
}
