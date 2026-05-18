package llm

type ChatReplyContext struct {
	SystemHint     string
	EarlierSummary string
	History        []Message
	UserMessage    Message
}
