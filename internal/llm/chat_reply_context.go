package llm

type ChatReplyContext struct {
	SystemHint     string
	EarlierSummary string
	History        []Message
	UserMessage    Message
	// Persona freezes the character for the whole turn: the template body
	// and defining settings captured from one operation snapshot. When set,
	// every generation iteration renders the persona from it instead of
	// resolving settings and the template body live, so the turn keeps
	// describing the character its operation started with. The dynamic
	// context hint and tool policy still compose per request. Nil keeps the
	// ordinary live rendering (explicit turns).
	Persona *CapturedPersona
}
