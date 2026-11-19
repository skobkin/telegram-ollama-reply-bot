package statistics

import (
	"errors"
	"time"

	"github.com/getsentry/sentry-go"
)

// GeneralSnapshot is the immutable general-statistics view: the request,
// feature-usage, and LLM-usage counters the bot has accumulated since the
// last general reset, plus the uptime string. The JSON tags and field
// order preserve the legacy /stats representation exactly.
type GeneralSnapshot struct {
	RunningSince    time.Time `json:"-"`
	PeriodStartedAt time.Time `json:"-"`
	Uptime          string    `json:"uptime"`

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

// generalCounters is the mutable general state guarded by the collector's
// mutex.
type generalCounters struct {
	groupRequests   uint64
	privateRequests uint64
	inlineQueries   uint64
	mentions        uint64
	summarize       uint64
	chatResets      uint64
	promptTokens    uint64
	completion      uint64
	totalTokens     uint64
	totalCost       float64
	llmTimeouts     uint64
}

// GeneralSnapshot deep-copies the general counters under the lock. Uptime
// is rendered from the process start — a general reset never restarts the
// uptime baseline, only the observation period.
func (c *Collector) GeneralSnapshot() GeneralSnapshot {
	c.mu.Lock()
	defer c.mu.Unlock()

	return GeneralSnapshot{
		RunningSince:      c.processStartedAt,
		PeriodStartedAt:   c.generalPeriodStartedAt,
		Uptime:            c.now().Sub(c.processStartedAt).String(),
		GroupRequests:     c.general.groupRequests,
		PrivateRequests:   c.general.privateRequests,
		InlineQueries:     c.general.inlineQueries,
		Mentions:          c.general.mentions,
		SummarizeRequests: c.general.summarize,
		ChatHistoryResets: c.general.chatResets,
		PromptTokens:      c.general.promptTokens,
		CompletionTokens:  c.general.completion,
		TotalTokens:       c.general.totalTokens,
		TotalCost:         c.general.totalCost,
		LlmTimeouts:       c.general.llmTimeouts,
	}
}

// ResetGeneral zeroes the general counters and restarts their observation
// period. The process-uptime baseline is untouched, and the interaction
// dataset (a separate collector section) is never affected.
func (c *Collector) ResetGeneral() {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.general = generalCounters{}
	c.generalPeriodStartedAt = c.now()
}

// GroupRequest records one group/supergroup request that addressed the bot.
func (c *Collector) GroupRequest() { c.bump(&c.general.groupRequests) }

// PrivateRequest records one private-chat request.
func (c *Collector) PrivateRequest() { c.bump(&c.general.privateRequests) }

// InlineQuery records one inline query routed to the bot.
func (c *Collector) InlineQuery() { c.bump(&c.general.inlineQueries) }

// Mention records one explicit mention request handled as a turn.
func (c *Collector) Mention() { c.bump(&c.general.mentions) }

// SummarizeRequest records one /summarize invocation.
func (c *Collector) SummarizeRequest() { c.bump(&c.general.summarize) }

// ChatHistoryReset records one chat-history reset (the /reset command).
func (c *Collector) ChatHistoryReset() { c.bump(&c.general.chatResets) }

// LlmTimeout records one generation request abandoned at the processing
// deadline.
func (c *Collector) LlmTimeout() { c.bump(&c.general.llmTimeouts) }

// AddUsage accumulates one LLM usage report. Negative token values are a
// contract violation and are rejected (reported to Sentry, not recorded).
func (c *Collector) AddUsage(prompt, completion, total int, cost float64) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if prompt < 0 || completion < 0 || total < 0 {
		sentry.CaptureException(errors.New("statistics: negative token usage"))

		return
	}

	c.general.promptTokens += uint64(prompt)
	c.general.completion += uint64(completion)
	c.general.totalTokens += uint64(total)
	c.general.totalCost += cost
}

// bump increments one general counter under the lock.
func (c *Collector) bump(field *uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()

	*field++
}
