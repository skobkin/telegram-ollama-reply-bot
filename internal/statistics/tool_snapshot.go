package statistics

import (
	"sort"
	"time"
)

// ToolPeriod is the observation period of the tool-usage dataset.
type ToolPeriod struct {
	StartedAt time.Time `json:"started_at"`
	Resets    uint64    `json:"resets"`
}

// ToolCounters are the per-row scalar counters. All turn counters are
// UNIQUE turns per row (deduplicated after folding); the two identity-turn
// counters count (original identity, turn) pairs, so on folded rows several
// identities can contribute within one turn.
type ToolCounters struct {
	AdvertisedTurns         uint64 `json:"advertised_turns"`
	AdvertisedInvokedTurns  uint64 `json:"advertised_invoked_turns"`
	InvokedTurns            uint64 `json:"invoked_turns"`
	Attempts                uint64 `json:"attempts"`
	Executed                uint64 `json:"executed"`
	Succeeded               uint64 `json:"succeeded"`
	SucceededEmpty          uint64 `json:"succeeded_empty"`
	Failed                  uint64 `json:"failed"`
	Rejected                uint64 `json:"rejected"`
	Retries                 uint64 `json:"retries_after_error"`
	RecoveredCalls          uint64 `json:"recovered_calls"`
	FailureIdentityTurns    uint64 `json:"failure_identity_turns"`
	RecoveredIdentityTurns  uint64 `json:"recovered_identity_turns"`
	PreparationReduced      uint64 `json:"result_preparation_reduced"`
	PreparationFailed       uint64 `json:"result_preparation_failed"`
	RepeatsAfterPrepFailure uint64 `json:"repeats_after_preparation_failure"`
	StatusUnrecognized      uint64 `json:"status_unrecognized"`
}

// ToolRejections breaks down calls rejected before handler execution.
type ToolRejections struct {
	NotRegistered uint64 `json:"not_registered"`
	NotAdvertised uint64 `json:"not_advertised"`
	Unavailable   uint64 `json:"unavailable"`
}

// ToolSideEffects counts side-effect outcomes of side-effecting calls only.
// Confirmed effects are never erased by a later result-preparation failure
// or a failed turn.
type ToolSideEffects struct {
	Confirmed  uint64 `json:"confirmed"`
	NotApplied uint64 `json:"not_applied"`
	Unknown    uint64 `json:"unknown"`
}

// ToolErrorClasses counts rejected and executed-failed calls per stable
// error class.
type ToolErrorClasses struct {
	InvalidArguments uint64 `json:"invalid_arguments"`
	NotFound         uint64 `json:"not_found"`
	Unavailable      uint64 `json:"unavailable"`
	RateLimited      uint64 `json:"rate_limited"`
	Permanent        uint64 `json:"permanent"`
	Transient        uint64 `json:"transient"`
	Cancelled        uint64 `json:"cancelled"`
	AmbiguousOutcome uint64 `json:"ambiguous_outcome"`
	Other            uint64 `json:"other"`
}

// ToolErrorCodeCount is one admitted error code, or the tagged bucket that
// collects every folded or invalid code. The bucket has a nil Code and
// Overflow set, so it can never collide with an admitted code that is
// literally named "other".
type ToolErrorCodeCount struct {
	Code     *string `json:"code"`
	Overflow bool    `json:"overflow,omitempty"`
	Count    uint64  `json:"count"`
}

// ToolRates are derived ratios; nil (JSON null) when the denominator is 0.
type ToolRates struct {
	// SuccessRate is succeeded / attempts.
	SuccessRate *float64 `json:"success_rate"`
	// UtilizationRate is advertised_invoked_turns / advertised_turns: the
	// share of turns advertising the tool in which it was also invoked. It
	// never exceeds 1 because calls to tools that were not advertised are not
	// in the numerator.
	UtilizationRate *float64 `json:"utilization_rate"`
	// ExecutionFailureRate is failed / executed.
	ExecutionFailureRate *float64 `json:"execution_failure_rate"`
	// RecoveryRate is recovered_identity_turns / failure_identity_turns.
	RecoveryRate *float64 `json:"recovery_rate"`
}

// ToolStats is the full statistics block of a row or of the dataset total.
type ToolStats struct {
	Counters     ToolCounters         `json:"counters"`
	Rejections   ToolRejections       `json:"rejections"`
	SideEffects  ToolSideEffects      `json:"side_effects"`
	ErrorClasses ToolErrorClasses     `json:"error_classes"`
	ErrorCodes   []ToolErrorCodeCount `json:"error_codes"`
	Rates        ToolRates            `json:"rates"`
	Latency      HistogramSnapshot    `json:"latency_ms"`
	Percentiles  LatencyPercentiles   `json:"latency_percentiles_approx"`
}

// ToolModeCounts is the compact per-mode split of one tool row.
type ToolModeCounts struct {
	AdvertisedTurns uint64 `json:"advertised_turns"`
	Attempts        uint64 `json:"attempts"`
	Succeeded       uint64 `json:"succeeded"`
	Unsuccessful    uint64 `json:"unsuccessful"`
}

// ToolModes splits a row between explicit and proactive generation.
type ToolModes struct {
	Explicit  ToolModeCounts `json:"explicit"`
	Proactive ToolModeCounts `json:"proactive"`
}

// ToolRowView is one tool's row. Tool is nil for the tagged overflow rows
// (other_registered, unregistered); those never carry a name.
type ToolRowView struct {
	Tool     *string `json:"tool"`
	Overflow string  `json:"overflow,omitempty"`
	// Registered is false for a tool that has persisted statistics but is no
	// longer in the registered catalog.
	Registered bool `json:"registered"`
	// NeverAdvertised: no turn advertised the tool (it may still have been
	// called by the model and rejected). AdvertisedUnused: advertised in at
	// least one turn, but never invoked.
	NeverAdvertised  bool `json:"never_advertised"`
	AdvertisedUnused bool `json:"advertised_unused"`
	ToolStats
	Modes ToolModes `json:"modes"`
}

// ToolTurnView is the turn-level context of one mode.
type ToolTurnView struct {
	Mode           string `json:"mode"`
	Turns          uint64 `json:"turns"`
	TurnsWithCalls uint64 `json:"turns_with_calls"`
	// RunFailures cover generation/runtime failures at the observation point
	// (before final Telegram text delivery, which interaction execution
	// statistics own). Tool statistics of failed turns are retained.
	RunFailures ToolRunFailures `json:"run_failures"`
}

// ToolRunFailures counts turns by generation/runtime failure class.
type ToolRunFailures struct {
	Timeout    uint64 `json:"timeout"`
	Cancelled  uint64 `json:"cancelled"`
	Generation uint64 `json:"generation"`
}

// ToolModelView is the model×mode attribution of the tool dataset (global
// scope only: the per-chat projection carries no model dimension).
type ToolModelView struct {
	Model         *string `json:"model"`
	ModelOverflow bool    `json:"model_overflow"`
	Mode          string  `json:"mode"`
	Turns         uint64  `json:"turns"`
	Attempts      uint64  `json:"attempts"`
	Succeeded     uint64  `json:"succeeded"`
	Unsuccessful  uint64  `json:"unsuccessful"`
}

// ToolCatalogInfo discloses what the catalog and the bounds omit.
type ToolCatalogInfo struct {
	Size    int `json:"size"`
	Tracked int `json:"tracked"`
	// Omitted catalog tools hold no row of their own: the tracked-tool bound
	// folded them into the other_registered row.
	Omitted int `json:"omitted"`
}

// ToolSnapshot is the immutable tool-usage view of one instant. The JSON
// and PNG presenters consume this one value.
type ToolSnapshot struct {
	GeneratedAt time.Time       `json:"generated_at"`
	Period      ToolPeriod      `json:"observation_period"`
	Persistence PersistenceInfo `json:"persistence"`

	Turns  []ToolTurnView `json:"turns"`
	Totals ToolStats      `json:"totals"`
	Tools  []ToolRowView  `json:"tools"`
	// Models is nil under chat scope: model attribution is global-only.
	Models  []ToolModelView `json:"models"`
	Catalog ToolCatalogInfo `json:"catalog"`

	// PerChatComplete is false when chat overflow rows exist: per-chat
	// attribution is then incomplete (folded observations cannot be
	// attributed to a chat).
	PerChatComplete bool `json:"per_chat_complete"`
	// ModelAttributionComplete is false when model overflow rows exist.
	ModelAttributionComplete bool   `json:"model_attribution_complete"`
	OversizedIDFolds         uint64 `json:"oversized_id_folds"`
	// UntrackedSelection is set when the selected chat holds no tracked
	// rows while overflow folding is active.
	UntrackedSelection string `json:"untracked_selection"`
}

func ratio(num, den uint64) *float64 {
	if den == 0 {
		return nil
	}
	v := float64(num) / float64(den)

	return &v
}

func toolStatsOf(row *toolRow, codeNames []string) ToolStats {
	c := row.c
	stats := ToolStats{
		Counters: ToolCounters{
			AdvertisedTurns:         c[tcAdvertisedTurns],
			AdvertisedInvokedTurns:  c[tcAdvertisedInvokedTurns],
			InvokedTurns:            c[tcInvokedTurns],
			Attempts:                c[tcAttempts],
			Executed:                c[tcExecuted],
			Succeeded:               c[tcSucceeded],
			SucceededEmpty:          c[tcSucceededEmpty],
			Failed:                  c[tcFailed],
			Rejected:                c[tcRejectedNotRegistered] + c[tcRejectedNotAdvertised] + c[tcRejectedUnavailable],
			Retries:                 c[tcRetries],
			RecoveredCalls:          c[tcRecoveredCalls],
			FailureIdentityTurns:    c[tcFailureIdentityTurns],
			RecoveredIdentityTurns:  c[tcRecoveredIdentityTurns],
			PreparationReduced:      c[tcPrepReduced],
			PreparationFailed:       c[tcPrepFailed],
			RepeatsAfterPrepFailure: c[tcRepeatsAfterPrepFailure],
			StatusUnrecognized:      c[tcStatusUnrecognized],
		},
		Rejections: ToolRejections{
			NotRegistered: c[tcRejectedNotRegistered],
			NotAdvertised: c[tcRejectedNotAdvertised],
			Unavailable:   c[tcRejectedUnavailable],
		},
		SideEffects: ToolSideEffects{
			Confirmed:  c[tcSideConfirmed],
			NotApplied: c[tcSideNotApplied],
			Unknown:    c[tcSideUnknown],
		},
		ErrorClasses: ToolErrorClasses{
			InvalidArguments: row.classes[0], NotFound: row.classes[1], Unavailable: row.classes[2],
			RateLimited: row.classes[3], Permanent: row.classes[4], Transient: row.classes[5],
			Cancelled: row.classes[6], AmbiguousOutcome: row.classes[7], Other: row.classes[8],
		},
		ErrorCodes: []ToolErrorCodeCount{},
		Rates: ToolRates{
			SuccessRate:          ratio(c[tcSucceeded], c[tcAttempts]),
			UtilizationRate:      ratio(c[tcAdvertisedInvokedTurns], c[tcAdvertisedTurns]),
			ExecutionFailureRate: ratio(c[tcFailed], c[tcExecuted]),
			RecoveryRate:         ratio(c[tcRecoveredIdentityTurns], c[tcFailureIdentityTurns]),
		},
		Latency:     latencySnapshot(row.latency),
		Percentiles: percentilesOf(row.latency),
	}
	for id, n := range row.codes {
		if id == otherCodeID || int(id) >= len(codeNames) {
			stats.ErrorCodes = append(stats.ErrorCodes, ToolErrorCodeCount{Overflow: true, Count: n})

			continue
		}
		name := codeNames[id]
		stats.ErrorCodes = append(stats.ErrorCodes, ToolErrorCodeCount{Code: &name, Count: n})
	}
	sort.Slice(stats.ErrorCodes, func(i, j int) bool {
		a, b := stats.ErrorCodes[i], stats.ErrorCodes[j]
		if a.Count != b.Count {
			return a.Count > b.Count
		}

		if a.Overflow != b.Overflow {
			return !a.Overflow // the folded bucket sorts after named codes
		}
		if a.Code != nil && b.Code != nil {
			return *a.Code < *b.Code
		}

		return false
	})

	return stats
}

func (c *Collector) toolTrackedCatalogLocked() (tracked map[string]struct{}, size, trackedCount int) {
	tracked = make(map[string]struct{}, len(c.toolCatalog))
	for _, name := range c.toolCatalog {
		tracked[name] = struct{}{}
		if _, ok := c.tool.trackedTools[name]; ok {
			trackedCount++
		}
	}

	return tracked, len(tracked), trackedCount
}

// ToolSnapshot builds the immutable tool-usage view for a scope, deep-copying
// under the collector lock.
func (c *Collector) ToolSnapshot(scope InteractionScope) ToolSnapshot {
	c.mu.Lock()
	defer c.mu.Unlock()

	t := &c.tool
	snap := ToolSnapshot{
		GeneratedAt:              c.now(),
		Period:                   ToolPeriod{StartedAt: t.periodStartedAt, Resets: c.toolResets},
		Persistence:              c.toolPersist.info,
		PerChatComplete:          true,
		ModelAttributionComplete: true,
		OversizedIDFolds:         t.oversizedIDFolds,
	}

	// Select the projection rows: model projection for global scope (exact
	// totals, model attribution), chat projection for a chat scope.
	type selected struct {
		row  *toolRow
		mode ToolMode
	}
	perTool := map[toolKey][]selected{}
	total := newToolRow()
	var turnByMode [toolModeCount]toolTurnCounters

	if scope.ChatSpecified {
		for key, row := range t.byChat {
			if key.chat.overflow || key.chat.id != scope.ChatID {
				continue
			}
			perTool[key.tool] = append(perTool[key.tool], selected{row, key.mode})
			total.add(row)
		}
		for key, turns := range t.turnsByChat {
			if key.chat.overflow || key.chat.id != scope.ChatID {
				continue
			}
			turnByMode[key.mode].add(turns)
		}
		if _, tracked := t.trackedChats[scope.ChatID]; !tracked && c.toolChatOverflowLocked() {
			snap.UntrackedSelection = "no tracked observations match this chat; activity may be folded into the overflow rows"
		}
	} else {
		for key, row := range t.byModel {
			perTool[key.tool] = append(perTool[key.tool], selected{row, key.mode})
			total.add(row)
			if key.model.overflow {
				snap.ModelAttributionComplete = false
			}
		}
		for key, turns := range t.turnsByModel {
			turnByMode[key.mode].add(turns)
		}
	}
	snap.PerChatComplete = !c.toolChatOverflowLocked()

	for mode := ToolMode(0); mode < toolModeCount; mode++ {
		tc := turnByMode[mode]
		snap.Turns = append(snap.Turns, ToolTurnView{
			Mode: mode.String(), Turns: tc.turns, TurnsWithCalls: tc.withCalls,
			RunFailures: ToolRunFailures{Timeout: tc.failures[0], Cancelled: tc.failures[1], Generation: tc.failures[2]},
		})
	}
	snap.Totals = toolStatsOf(total, t.codeNames)

	catalogSet, catalogSize, catalogTracked := c.toolTrackedCatalogLocked()
	snap.Catalog = ToolCatalogInfo{Size: catalogSize, Tracked: catalogTracked, Omitted: catalogSize - catalogTracked}

	// Rows: every tool with data, plus every tracked catalog tool (zero use).
	keys := make([]toolKey, 0, len(perTool)+len(t.trackedTools))
	seen := map[toolKey]bool{}
	for key := range perTool {
		keys = append(keys, key)
		seen[key] = true
	}
	for name := range t.trackedTools {
		if _, inCatalog := catalogSet[name]; !inCatalog {
			continue
		}
		key := toolKey{kind: toolTracked, name: name}
		if !seen[key] {
			keys = append(keys, key)
		}
	}
	for _, key := range keys {
		row := newToolRow()
		var modes ToolModes
		for _, sel := range perTool[key] {
			row.add(sel.row)
			counts := &modes.Explicit
			if sel.mode == ToolModeProactive {
				counts = &modes.Proactive
			}
			counts.AdvertisedTurns += sel.row.c[tcAdvertisedTurns]
			counts.Attempts += sel.row.c[tcAttempts]
			counts.Succeeded += sel.row.c[tcSucceeded]
			counts.Unsuccessful += sel.row.c[tcFailed] + sel.row.c[tcRejectedNotRegistered] +
				sel.row.c[tcRejectedNotAdvertised] + sel.row.c[tcRejectedUnavailable]
		}
		view := ToolRowView{ToolStats: toolStatsOf(row, t.codeNames), Modes: modes}
		switch key.kind {
		case toolTracked:
			name := key.name
			view.Tool = &name
			_, view.Registered = catalogSet[name]
		case toolOtherRegistered:
			view.Overflow = "other_registered"
			view.Registered = true
		case toolUnregistered:
			view.Overflow = "unregistered"
		}
		view.NeverAdvertised = row.c[tcAdvertisedTurns] == 0
		view.AdvertisedUnused = row.c[tcAdvertisedTurns] > 0 && row.c[tcAttempts] == 0
		snap.Tools = append(snap.Tools, view)
	}
	sortToolRows(snap.Tools)

	if !scope.ChatSpecified {
		snap.Models = c.toolModelViewsLocked()
	}

	return snap
}

func (c *Collector) toolChatOverflowLocked() bool {
	for key, row := range c.tool.byChat {
		if key.chat.overflow && row.c[tcAttempts]+row.c[tcAdvertisedTurns] > 0 {
			return true
		}
	}
	for key, turns := range c.tool.turnsByChat {
		if key.chat.overflow && turns.turns > 0 {
			return true
		}
	}

	return false
}

func (c *Collector) toolModelViewsLocked() []ToolModelView {
	type agg struct{ attempts, succeeded, unsuccessful uint64 }
	aggs := map[toolTurnModelKey]*agg{}
	for key, row := range c.tool.byModel {
		k := toolTurnModelKey{model: key.model, mode: key.mode}
		a := aggs[k]
		if a == nil {
			a = &agg{}
			aggs[k] = a
		}
		a.attempts += row.c[tcAttempts]
		a.succeeded += row.c[tcSucceeded]
		a.unsuccessful += row.c[tcFailed] + row.c[tcRejectedNotRegistered] + row.c[tcRejectedNotAdvertised] + row.c[tcRejectedUnavailable]
	}
	views := make([]ToolModelView, 0, len(c.tool.turnsByModel))
	for key, turns := range c.tool.turnsByModel {
		a := aggs[key]
		if a == nil {
			a = &agg{}
		}
		views = append(views, ToolModelView{
			Model: modelPointer(key.model), ModelOverflow: key.model.overflow, Mode: key.mode.String(),
			Turns: turns.turns, Attempts: a.attempts, Succeeded: a.succeeded, Unsuccessful: a.unsuccessful,
		})
	}
	sort.Slice(views, func(i, j int) bool {
		a, b := views[i], views[j]
		if a.ModelOverflow != b.ModelOverflow {
			return overflowRank(b.ModelOverflow) < overflowRank(a.ModelOverflow)
		}
		if modelSortName(a.Model) != modelSortName(b.Model) {
			return modelSortName(a.Model) < modelSortName(b.Model)
		}

		return a.Mode < b.Mode
	})

	return views
}

// sortToolRows orders by attempts desc, advertised turns desc, then name
// (overflow rows after named ones).
func sortToolRows(rows []ToolRowView) {
	name := func(r ToolRowView) string {
		if r.Tool != nil {
			return *r.Tool
		}

		return "\xff" + r.Overflow
	}
	sort.Slice(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if a.Counters.Attempts != b.Counters.Attempts {
			return a.Counters.Attempts > b.Counters.Attempts
		}
		if a.Counters.AdvertisedTurns != b.Counters.AdvertisedTurns {
			return a.Counters.AdvertisedTurns > b.Counters.AdvertisedTurns
		}

		return name(a) < name(b)
	})
}
