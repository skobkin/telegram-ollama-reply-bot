package state

import (
	"encoding/json"
	"time"
)

type ConversationScope struct {
	ChatID  int64
	TopicID int
}

type ImageMeta struct {
	FileID       string
	FileUniqueID string
	Width        int
	Height       int
	FileSize     int
}

func (i *ImageMeta) CacheKey() string {
	if i == nil {
		return ""
	}
	if i.FileUniqueID != "" {
		return i.FileUniqueID
	}

	return i.FileID
}

type Message struct {
	Name          string
	Username      string
	Text          string
	IsMe          bool
	IsUserRequest bool
	ReplyTo       *Message
	HasImage      bool
	Image         string
	ImageMeta     *ImageMeta
	ChatID        int64
	TopicID       int
	MessageID     int
	FromID        int64
	CreatedAt     time.Time
}

type ConversationSnapshot struct {
	Messages            []Message
	EarlierSummary      string
	SummaryMessageCount int
	MessageCount        int
	ApproxBytes         int64
	LastUpdatedAtUTC    time.Time
}

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

type ConversationStore interface {
	AppendMessage(scope ConversationScope, msg Message)
	Snapshot(scope ConversationScope) ConversationSnapshot
	Search(scope ConversationScope, query HistorySearchQuery) []HistorySearchMatch
	SetEarlierSummary(scope ConversationScope, text string, summaryMessageCount int)
	Reset(scope ConversationScope)
	ResetChat(chatID int64)
}

type ImageStore interface {
	Get(imageMeta *ImageMeta) (string, bool)
	Set(imageMeta *ImageMeta, description string)
}

type UsageTotals struct {
	PromptTokens     uint64  `json:"prompt_tokens"`
	CompletionTokens uint64  `json:"completion_tokens"`
	TotalTokens      uint64  `json:"total_tokens"`
	TotalCost        float64 `json:"total_cost"`
}

type StatsSnapshot struct {
	RunningSince time.Time `json:"-"`
	Uptime       string    `json:"uptime"`

	GroupRequests   uint64 `json:"group_requests"`
	PrivateRequests uint64 `json:"private_requests"`
	InlineQueries   uint64 `json:"inline_queries"`

	Mentions          uint64 `json:"mentions"`
	SummarizeRequests uint64 `json:"summarize_requests"`
	ChatHistoryResets uint64 `json:"chat_history_resets"`

	PromptTokens     uint64  `json:"prompt_tokens"`
	CompletionTokens uint64  `json:"completion_tokens"`
	TotalTokens      uint64  `json:"total_tokens"`
	TotalCost        float64 `json:"total_cost"`
	LlmTimeouts      uint64  `json:"llm_timeouts"`
}

func (s StatsSnapshot) String() string {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return "{\"error\": \"cannot serialize stats\"}"
	}

	return string(data)
}

type StatsStore interface {
	Snapshot() StatsSnapshot
	String() string
	InlineQuery()
	GroupRequest()
	PrivateRequest()
	Mention()
	SummarizeRequest()
	ChatHistoryReset()
	AddUsage(prompt, completion, total int, cost float64)
	LlmTimeout()
}
