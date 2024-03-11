package stats

import (
	"encoding/json"
	"sync"
	"time"
)

type Stats struct {
	mu sync.Mutex

	RunningSince time.Time

	GroupRequests   uint64
	PrivateRequests uint64

	HeyRequests       uint64
	SummarizeRequests uint64
}

func NewStats() *Stats {
	return &Stats{
		RunningSince: time.Now(),

		GroupRequests:   0,
		PrivateRequests: 0,

		HeyRequests:       0,
		SummarizeRequests: 0,
	}
}

func (s *Stats) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Uptime string `json:"uptime"`

		GroupRequests   uint64 `json:"group_requests"`
		PrivateRequests uint64 `json:"private_requests"`

		HeyRequests       uint64 `json:"hey_requests"`
		SummarizeRequests uint64 `json:"summarize_requests"`
	}{
		Uptime: time.Now().Sub(s.RunningSince).String(),

		GroupRequests:   s.GroupRequests,
		PrivateRequests: s.PrivateRequests,

		HeyRequests:       s.HeyRequests,
		SummarizeRequests: s.SummarizeRequests,
	})
}

func (s *Stats) String() string {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return "{\"error\": \"cannot serialize stats\"}"
	}

	return string(data)
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

func (s *Stats) HeyRequest() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.HeyRequests++
}

func (s *Stats) SummarizeRequest() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.SummarizeRequests++
}
