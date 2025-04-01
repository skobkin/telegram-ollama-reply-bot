package stats

import (
	"encoding/json"
	"sync"
	"time"

	"github.com/getsentry/sentry-go"
)

type Stats struct {
	mu sync.Mutex

	RunningSince time.Time

	GroupRequests   uint64
	PrivateRequests uint64
	InlineQueries   uint64

	Mentions          uint64
	SummarizeRequests uint64
	ChatHistoryResets uint64
}

func NewStats() *Stats {
	return &Stats{
		RunningSince: time.Now(),

		GroupRequests:   0,
		PrivateRequests: 0,
		InlineQueries:   0,

		Mentions:          0,
		SummarizeRequests: 0,
		ChatHistoryResets: 0,
	}
}

func (s *Stats) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Uptime string `json:"uptime"`

		GroupRequests   uint64 `json:"group_requests"`
		PrivateRequests uint64 `json:"private_requests"`
		InlineQueries   uint64 `json:"inline_queries"`

		Mentions          uint64 `json:"mentions"`
		SummarizeRequests uint64 `json:"summarize_requests"`
		ChatHistoryResets uint64 `json:"chat_history_resets"`
	}{
		Uptime: time.Now().Sub(s.RunningSince).String(),

		GroupRequests:   s.GroupRequests,
		PrivateRequests: s.PrivateRequests,
		InlineQueries:   s.InlineQueries,

		Mentions:          s.Mentions,
		SummarizeRequests: s.SummarizeRequests,
		ChatHistoryResets: s.ChatHistoryResets,
	})
}

func (s *Stats) String() string {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		sentry.CaptureException(err)

		return "{\"error\": \"cannot serialize stats\"}"
	}

	return string(data)
}

func (s *Stats) InlineQuery() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.InlineQueries++
}

func (s *Stats) GroupRequest() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.GroupRequests++
}

func (s *Stats) PrivateRequest() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.PrivateRequests++
}

func (s *Stats) Mention() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Mentions++
}

func (s *Stats) SummarizeRequest() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.SummarizeRequests++
}

func (s *Stats) ChatHistoryReset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ChatHistoryResets++
}
