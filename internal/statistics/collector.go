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
	// RubricVersion stamps interaction snapshots with the decision rubric
	// version the app was built against (injected by the app wiring).
	RubricVersion int
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

	rubricVersion int

	processStartedAt time.Time

	general                generalCounters
	generalPeriodStartedAt time.Time

	interaction interactionState
	// interactionPersist carries the reset generation (the statistics
	// epoch, monotonic across restarts once restored) and the revision used
	// for dirty tracking.
	interactionPersist persistTracker
	interactionResets  uint64

	// The tool-usage dataset is persistent like the interaction one but has
	// its own reset generation, so the resets stay independent.
	tool        toolState
	toolPersist persistTracker
	toolResets  uint64
	toolCatalog []string
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
		rubricVersion:          cfg.RubricVersion,
		processStartedAt:       started,
		generalPeriodStartedAt: started,
		interaction:            initInteraction(started),
		interactionPersist:     newPersistTracker(),
		tool:                   initTool(started),
		toolPersist:            newPersistTracker(),
	}
}
