package state

type HistorySearchMatchMode string

const (
	HistorySearchMatchModeAll HistorySearchMatchMode = "all"
	HistorySearchMatchModeAny HistorySearchMatchMode = "any"
)

type HistorySearchQuery struct {
	Keywords  []string
	MatchMode HistorySearchMatchMode
	Limit     int
}

type HistorySearchMatch struct {
	Message         Message
	Score           int
	MatchKind       string
	MatchedKeywords []string
}
