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
	Reactions     []Reaction
}

// Reaction is a single user's reaction on one of the bot's own messages.
// Only reactions on bot messages are observable by bots, and Telegram
// reports each user's current reaction set in full, so reactions are
// stored per user and replaced wholesale on every update.
type Reaction struct {
	UserID    int64
	Name      string
	Username  string
	Emoji     string
	UpdatedAt time.Time
}

type ConversationSnapshot struct {
	Messages            []Message
	EarlierSummary      string
	SummaryMessageCount int
	MessageCount        int
	ApproxBytes         int64
	LastUpdatedAtUTC    time.Time
	// Revision is the version of the raw history this snapshot was taken at.
	// Raw-history mutations that can make derived state stale (reaction
	// changes, head trims) bump it, so a summary generated from this
	// snapshot can be committed only against an unchanged revision.
	Revision int64
}

type ConversationStore interface {
	AppendMessage(scope ConversationScope, msg Message)
	Snapshot(scope ConversationScope) ConversationSnapshot
	Search(scope ConversationScope, query HistorySearchQuery) []HistorySearchMatch
	// SetEarlierSummary stores a summary covering the first
	// summaryMessageCount raw messages. historyRevision must be the
	// Revision of the snapshot the summary was rendered from: if the raw
	// history changed in the meantime (reaction change, head trim) or the
	// stream no longer exists (reset, eviction), the commit is rejected and
	// returns false instead of restoring stale derived state.
	SetEarlierSummary(scope ConversationScope, text string, summaryMessageCount int, historyRevision int64) bool
	// SetMessageReactions atomically replaces all reactions of a single user
	// on one of the bot's own messages (IsMe) identified by its Telegram
	// message ID within the chat. Reaction updates carry no topic, so all
	// topic streams of the chat are searched; Telegram message IDs are
	// unique per chat. updateSeq is the sequential Telegram update ID of the
	// reaction update: concurrent handlers do not preserve arrival order, so
	// a call whose updateSeq is not newer than the last applied one for this
	// user and message is rejected as stale, including after removals. An
	// empty reactions slice removes that user's reactions. Returns false
	// when the target message is not in history (restart, trim, reset, or a
	// reaction on a non-bot message).
	SetMessageReactions(chatID int64, messageID int, userID int64, updateSeq int64, reactions []Reaction) bool
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
