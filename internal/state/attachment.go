package state

import "strings"

// AttachmentKind names the kind of non-text content a history message carries.
// Photos are not an attachment: they keep their dedicated HasImage/ImageMeta
// representation, which the vision pipeline relies on.
type AttachmentKind string

const (
	AttachmentSticker   AttachmentKind = "sticker"
	AttachmentPoll      AttachmentKind = "poll"
	AttachmentQuiz      AttachmentKind = "quiz"
	AttachmentDice      AttachmentKind = "dice"
	AttachmentChart     AttachmentKind = "chart"
	AttachmentAnimation AttachmentKind = "animation"
	AttachmentVideo     AttachmentKind = "video"
	AttachmentVideoNote AttachmentKind = "video_note"
	AttachmentVoice     AttachmentKind = "voice"
	AttachmentAudio     AttachmentKind = "audio"
	AttachmentDocument  AttachmentKind = "document"
	AttachmentLocation  AttachmentKind = "location"
	AttachmentContact   AttachmentKind = "contact"
)

// Attachment is a concise, text-only description of the non-text content of a
// history message (a sticker, poll, dice roll, chart, voice note, and so on).
// It exists so the model can read and reference those messages in context,
// search results and summaries. It deliberately holds no binary data and no
// file identifiers: history stays small, in memory only, and the content
// itself is never retained or re-fetched because of it.
type Attachment struct {
	Kind AttachmentKind
	// Label is the short human-readable description, already bounded and free
	// of control characters (for example `😀 cat waving` or `"Lunch?" — Pizza
	// / Sushi`). It must not carry secrets or full payloads.
	Label string
}

// Placeholder renders the attachment the way it appears inline in prompts,
// search text and summaries: `[Sticker: 😀 cat waving]`. It returns "" for a
// nil attachment, so callers can write it unconditionally. This is the single
// rendering of attachments; every consumer uses it so the representations
// cannot drift apart.
func (a *Attachment) Placeholder() string {
	if a == nil || a.Kind == "" {
		return ""
	}

	name := attachmentDisplayName(a.Kind)
	if a.Label == "" {
		return "[" + name + "]"
	}

	return "[" + name + ": " + a.Label + "]"
}

// sendTools maps the kinds the bot can send itself to the tool that sends them.
var sendTools = map[AttachmentKind]struct{ noun, tool string }{
	AttachmentSticker: {"sticker", "send_sticker"},
	AttachmentPoll:    {"poll", "send_poll"},
	AttachmentQuiz:    {"quiz", "send_quiz"},
	AttachmentDice:    {"dice roll", "send_dice"},
	AttachmentChart:   {"chart", "generate_chart"},
}

// SentNarration renders a bot-authored attachment as an action the bot took:
// `(sent a sticker via send_sticker: 😀 cat waving)`. Unlike Placeholder it
// never looks like message content. In the assistant turns of a prompt a
// bracketed placeholder is something the model "said", and models copy the
// format of their own earlier turns, writing `[Sticker: …]` as text instead of
// calling the tool. Kinds the bot cannot send itself fall back to Placeholder.
func (a *Attachment) SentNarration() string {
	if a == nil || a.Kind == "" {
		return ""
	}
	tool, ok := sendTools[a.Kind]
	if !ok {
		return a.Placeholder()
	}

	narration := "(sent a " + tool.noun + " via " + tool.tool
	if a.Label != "" {
		narration += ": " + a.Label
	}

	return narration + ")"
}

func attachmentDisplayName(kind AttachmentKind) string {
	switch kind {
	case AttachmentVideoNote:
		return "Video note"
	default:
		name := string(kind)

		return strings.ToUpper(name[:1]) + name[1:]
	}
}
