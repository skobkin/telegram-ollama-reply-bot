// Package statistics holds the bot's aggregate, in-memory statistics: the
// general request/usage counters and the System One interaction outcomes.
// The collector is a single concurrency-safe aggregate store. It is
// RAM-only by design: nothing here is persisted, logged, or exported
// anywhere except through the explicit presenter functions and the admin
// export flow. Snapshot first, then render — the Collector itself has no
// serialization methods.
package statistics

import (
	"sync"
	"time"
)

// Config carries the collector's construction-time dependencies.
type Config struct {
	// Now overrides the wall clock (tests inject a controllable one).
	// Nil means time.Now.
	Now func() time.Time
}

// Collector is the aggregate statistics store. All Record methods are
// atomic; snapshots deep-copy under the same lock into immutable value
// types. Two clocks are maintained and never conflated:
// processStartedAt (set once at construction, never reset — the true
// process-uptime baseline) and the per-dataset observation-period starts
// (reset by their Reset methods).
type Collector struct {
	mu  sync.Mutex
	now func() time.Time

	processStartedAt time.Time

	general                generalCounters
	generalPeriodStartedAt time.Time
}

// NewCollector constructs the collector. Both observation periods start at
// construction.
func NewCollector(cfg Config) *Collector {
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	started := now()

	return &Collector{
		now:                    now,
		processStartedAt:       started,
		generalPeriodStartedAt: started,
	}
}
