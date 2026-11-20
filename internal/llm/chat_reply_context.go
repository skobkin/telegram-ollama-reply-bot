package llm

import "time"

type ChatReplyContext struct {
	SystemHint     string
	EarlierSummary string
	History        []Message
	UserMessage    Message
	// RetainedHistory describes the same raw snapshot used to select History.
	// Nil means unavailable; an empty snapshot has RetainedMessages == 0.
	RetainedHistory *ConversationMetadata
	// Persona freezes the character for the whole turn: the template body
	// and defining settings captured from one operation snapshot. When set,
	// every generation iteration renders the persona from it instead of
	// resolving settings and the template body live, so the turn keeps
	// describing the character its operation started with. The dynamic
	// context hint and tool policy still compose per request. Nil keeps the
	// ordinary live rendering (explicit turns).
	Persona *CapturedPersona
}

// ConversationMetadata describes retained raw history, not complete Telegram
// history. Missing bounds are represented by zero times, never year-one dates.
type ConversationMetadata struct {
	RetainedMessages int
	OldestMessageAt  time.Time
	NewestMessageAt  time.Time
}
