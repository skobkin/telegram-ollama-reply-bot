package statistics

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---- helpers ----

func okCall(name string) ToolCallObservation {
	return ToolCallObservation{Name: name, Executed: true, Status: "ok", Duration: 50 * time.Millisecond}
}

func emptyCall(name string) ToolCallObservation {
	return ToolCallObservation{Name: name, Executed: true, Status: "empty", Duration: 20 * time.Millisecond}
}

func errCall(name, code, class string) ToolCallObservation {
	return ToolCallObservation{Name: name, Executed: true, Status: "error", ErrorCode: code, ErrorClass: class, Duration: 30 * time.Millisecond}
}

func rejectedCall(name string, rejection ToolRejection) ToolCallObservation {
	return ToolCallObservation{Name: name, Rejection: rejection, Status: "error", ErrorCode: "tool_unavailable", ErrorClass: "unavailable"}
}

type turnOpt func(*ToolTurnObservation)

func inChat(id int64) turnOpt          { return func(o *ToolTurnObservation) { o.ChatID = id } }
func withModel(m string) turnOpt       { return func(o *ToolTurnObservation) { o.GenerationModel = m } }
func proactive() turnOpt               { return func(o *ToolTurnObservation) { o.Mode = ToolModeProactive } }
func failing(f ToolRunFailure) turnOpt { return func(o *ToolTurnObservation) { o.RunFailure = f } }

// recordTurn records one turn advertising the given tools and making calls.
func recordTurn(c *Collector, advertised []string, calls []ToolCallObservation, opts ...turnOpt) {
	obs := ToolTurnObservation{Epoch: c.ToolEpoch(), ChatID: 1, GenerationModel: "m1", Advertised: advertised, Calls: calls}
	for _, opt := range opts {
		opt(&obs)
	}
	c.RecordToolTurn(obs)
}

func findTool(t *testing.T, snap ToolSnapshot, name string) ToolRowView {
	t.Helper()
	for _, row := range snap.Tools {
		if row.Tool != nil && *row.Tool == name {
			return row
		}
	}
	t.Fatalf("tool %q not in snapshot", name)

	return ToolRowView{}
}

func findOverflow(t *testing.T, snap ToolSnapshot, kind string) ToolRowView {
	t.Helper()
	for _, row := range snap.Tools {
		if row.Overflow == kind {
			return row
		}
	}
	t.Fatalf("overflow row %q not in snapshot", kind)

	return ToolRowView{}
}

func approx(t *testing.T, got *float64, want float64, what string) {
	t.Helper()
	if got == nil {
		t.Fatalf("%s is null, want %v", what, want)
	}
	if d := *got - want; d > 1e-9 || d < -1e-9 {
		t.Fatalf("%s = %v, want %v", what, *got, want)
	}
}

func newToolCollector() *Collector { return newPersistTestCollector() }

// ---- counting semantics ----

func TestToolAdvertisementIsCountedOncePerTurn(t *testing.T) {
	c := newToolCollector()
	recordTurn(c, []string{"a", "b"}, []ToolCallObservation{okCall("a"), okCall("a")})
	recordTurn(c, []string{"a", "b"}, nil)
	recordTurn(c, []string{"a", "a", "b"}, nil) // a duplicate advertised name still counts once

	snap := c.ToolSnapshot(GlobalScope())
	a := findTool(t, snap, "a")
	if a.Counters.AdvertisedTurns != 3 || a.Counters.InvokedTurns != 1 || a.Counters.Attempts != 2 {
		t.Fatalf("a counters %+v", a.Counters)
	}
	approx(t, a.Rates.UtilizationRate, 1.0/3, "utilization")
	b := findTool(t, snap, "b")
	if b.Counters.AdvertisedTurns != 3 || !b.AdvertisedUnused || b.NeverAdvertised {
		t.Fatalf("b must be advertised-but-unused: %+v", b)
	}
}

func TestToolUtilizationNeverExceedsOneAndNullWhenUndefined(t *testing.T) {
	c := newToolCollector()
	// b is advertised in one turn (unused) and invoked while NOT advertised in
	// another; a is only ever invoked unadvertised.
	recordTurn(c, []string{"b"}, nil)
	recordTurn(c, []string{"x"}, []ToolCallObservation{rejectedCall("b", ToolRejectedNotAdvertised), rejectedCall("b", ToolRejectedNotAdvertised), rejectedCall("a", ToolRejectedNotAdvertised)})

	snap := c.ToolSnapshot(GlobalScope())
	b := findTool(t, snap, "b")
	if b.Counters.AdvertisedTurns != 1 || b.Counters.InvokedTurns != 1 || b.Counters.AdvertisedInvokedTurns != 0 || b.Counters.Attempts != 2 {
		t.Fatalf("b counters %+v", b.Counters)
	}
	approx(t, b.Rates.UtilizationRate, 0, "b utilization")
	a := findTool(t, snap, "a")
	if a.Rates.UtilizationRate != nil || !a.NeverAdvertised || a.Counters.Attempts != 1 {
		t.Fatalf("a: attempts without advertisement have an undefined utilization: %+v", a)
	}
	for _, row := range snap.Tools {
		if row.Rates.UtilizationRate != nil && *row.Rates.UtilizationRate > 1 {
			t.Fatalf("utilization above 100%% for %v", row.Tool)
		}
	}
}

func TestToolEmptyResultIsASuccessNotAFailure(t *testing.T) {
	c := newToolCollector()
	recordTurn(c, []string{"links"}, []ToolCallObservation{emptyCall("links")})

	row := findTool(t, c.ToolSnapshot(GlobalScope()), "links")
	if row.Counters.Succeeded != 1 || row.Counters.SucceededEmpty != 1 || row.Counters.Failed != 0 || row.Counters.Rejected != 0 {
		t.Fatalf("an empty result is a successful execution: %+v", row.Counters)
	}
	if row.ErrorClasses != (ToolErrorClasses{}) || len(row.ErrorCodes) != 0 {
		t.Fatalf("a success carries no error: %+v %+v", row.ErrorClasses, row.ErrorCodes)
	}
	approx(t, row.Rates.SuccessRate, 1, "success rate")
}

func TestToolRejectedVersusExecutedFailures(t *testing.T) {
	c := newToolCollector()
	recordTurn(c, []string{"t"}, []ToolCallObservation{
		rejectedCall("t", ToolRejectedUnavailable),
		rejectedCall("t", ToolRejectedNotAdvertised),
		errCall("t", "invalid_arguments", "invalid_arguments"),
		okCall("t"),
	})

	row := findTool(t, c.ToolSnapshot(GlobalScope()), "t")
	if row.Counters.Attempts != 4 || row.Counters.Executed != 2 || row.Counters.Rejected != 2 || row.Counters.Failed != 1 || row.Counters.Succeeded != 1 {
		t.Fatalf("counters %+v", row.Counters)
	}
	if row.Rejections != (ToolRejections{NotAdvertised: 1, Unavailable: 1}) {
		t.Fatalf("rejections %+v", row.Rejections)
	}
	if row.ErrorClasses.Unavailable != 2 || row.ErrorClasses.InvalidArguments != 1 {
		t.Fatalf("classes %+v", row.ErrorClasses)
	}
	// Latency only covers executed calls (rejected ones never ran).
	if total := histogramTotal(row.Latency); total != 2 {
		t.Fatalf("latency samples = %d, want the 2 executed calls", total)
	}
	approx(t, row.Rates.ExecutionFailureRate, 0.5, "execution failure rate")
	approx(t, row.Rates.SuccessRate, 0.25, "success rate")
}

func histogramTotal(h HistogramSnapshot) uint64 {
	var total uint64
	for _, n := range h.Counts {
		total += n
	}

	return total
}

func TestToolSayNothingIsASuccessWithoutAnInventedSideEffect(t *testing.T) {
	c := newToolCollector()
	call := okCall("say_nothing")
	call.SideEffectOutcome = "none"
	recordTurn(c, []string{"say_nothing"}, []ToolCallObservation{call})

	row := findTool(t, c.ToolSnapshot(GlobalScope()), "say_nothing")
	if row.Counters.Succeeded != 1 || row.SideEffects != (ToolSideEffects{}) {
		t.Fatalf("say_nothing: success, no side effect: %+v %+v", row.Counters, row.SideEffects)
	}
}

func TestToolRetryAndRecoveryAreObservableLaterCalls(t *testing.T) {
	cases := []struct {
		name                            string
		calls                           []ToolCallObservation
		retries, recoveredCalls         uint64
		failureIdentityTurns, recovered uint64
		recoveryRate                    *float64
	}{
		{"fail then success", []ToolCallObservation{errCall("t", "x", "transient"), okCall("t")}, 1, 1, 1, 1, ptr(1)},
		{"fail fail success", []ToolCallObservation{errCall("t", "x", "transient"), errCall("t", "x", "transient"), okCall("t")}, 2, 1, 1, 1, ptr(1)},
		{"fail only", []ToolCallObservation{errCall("t", "x", "transient")}, 0, 0, 1, 0, ptr(0)},
		{"success then fail is not a retry", []ToolCallObservation{okCall("t"), errCall("t", "x", "transient")}, 0, 0, 1, 0, ptr(0)},
		{"rejection then success", []ToolCallObservation{rejectedCall("t", ToolRejectedUnavailable), okCall("t")}, 1, 1, 1, 1, ptr(1)},
		{"two recoveries count one identity-turn", []ToolCallObservation{errCall("t", "x", "transient"), okCall("t"), errCall("t", "x", "transient"), okCall("t")}, 3, 2, 1, 1, ptr(1)},
		{"unrelated tool success is not recovery", []ToolCallObservation{errCall("t", "x", "transient"), okCall("u")}, 0, 0, 1, 0, ptr(0)},
		{"no failure", []ToolCallObservation{okCall("t"), okCall("t")}, 0, 0, 0, 0, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newToolCollector()
			recordTurn(c, []string{"t", "u"}, tc.calls)
			row := findTool(t, c.ToolSnapshot(GlobalScope()), "t")
			got := row.Counters
			if got.Retries != tc.retries || got.RecoveredCalls != tc.recoveredCalls ||
				got.FailureIdentityTurns != tc.failureIdentityTurns || got.RecoveredIdentityTurns != tc.recovered {
				t.Fatalf("counters %+v", got)
			}
			if (row.Rates.RecoveryRate == nil) != (tc.recoveryRate == nil) {
				t.Fatalf("recovery rate = %v, want %v", row.Rates.RecoveryRate, tc.recoveryRate)
			}
			if tc.recoveryRate != nil {
				approx(t, row.Rates.RecoveryRate, *tc.recoveryRate, "recovery rate")
			}
		})
	}
}

func ptr(v float64) *float64 { return &v }

func TestToolRecoveryAcrossGenerationIterationsInOneTurn(t *testing.T) {
	// Records arrive in execution order across iterations and batches; the
	// collector never sees the batch boundary, so both are covered.
	c := newToolCollector()
	recordTurn(c, []string{"t"}, []ToolCallObservation{errCall("t", "x", "transient")})
	recordTurn(c, []string{"t"}, []ToolCallObservation{errCall("t", "x", "transient"), okCall("t")})

	row := findTool(t, c.ToolSnapshot(GlobalScope()), "t")
	if row.Counters.FailureIdentityTurns != 2 || row.Counters.RecoveredIdentityTurns != 1 {
		t.Fatalf("recovery must be per turn: %+v", row.Counters)
	}
}

func TestToolFoldedIdentitiesNeverCrossRecover(t *testing.T) {
	c := newToolCollector()
	// Fill the tracked-tool cap so later registered names fold.
	names := make([]string, 0, MaxTrackedTools+3)
	for i := 0; i < MaxTrackedTools; i++ {
		names = append(names, fmt.Sprintf("tool_%03d", i))
	}
	recordTurn(c, names, nil)

	// x1 fails, x2 (a DIFFERENT folded identity) succeeds: not a recovery.
	recordTurn(c, []string{"x1", "x2"}, []ToolCallObservation{errCall("x1", "boom", "permanent"), okCall("x2")})
	other := findOverflow(t, c.ToolSnapshot(GlobalScope()), "other_registered")
	if other.Counters.RecoveredIdentityTurns != 0 || other.Counters.RecoveredCalls != 0 || other.Counters.Retries != 0 {
		t.Fatalf("a different folded identity succeeding is not recovery: %+v", other.Counters)
	}
	if other.Counters.FailureIdentityTurns != 1 {
		t.Fatalf("failure identity-turns %+v", other.Counters)
	}

	// Two folded identities failing in one turn: two identity-turns, but one
	// unique advertised/invoked turn each.
	recordTurn(c, []string{"y1", "y2"}, []ToolCallObservation{errCall("y1", "boom", "permanent"), errCall("y2", "boom", "permanent")})
	other = findOverflow(t, c.ToolSnapshot(GlobalScope()), "other_registered")
	if other.Counters.FailureIdentityTurns != 3 {
		t.Fatalf("failure identity-turns %d, want 3", other.Counters.FailureIdentityTurns)
	}
	if other.Counters.AdvertisedTurns != 2 || other.Counters.InvokedTurns != 2 || other.Counters.AdvertisedInvokedTurns != 2 {
		t.Fatalf("overflow turn counters must be deduplicated per turn: %+v", other.Counters)
	}

	// The same folded identity recovering does count.
	recordTurn(c, []string{"z"}, []ToolCallObservation{errCall("z", "boom", "permanent"), okCall("z")})
	other = findOverflow(t, c.ToolSnapshot(GlobalScope()), "other_registered")
	if other.Counters.RecoveredIdentityTurns != 1 || other.Counters.RecoveredCalls != 1 || other.Counters.Retries != 1 {
		t.Fatalf("same-identity recovery inside an overflow row: %+v", other.Counters)
	}
}

func TestToolUnregisteredCallsFoldAndNeverBecomeKeys(t *testing.T) {
	c := newToolCollector()
	recordTurn(c, []string{"real"}, []ToolCallObservation{
		rejectedCall("hallucinated_1", ToolRejectedNotRegistered),
		rejectedCall("hallucinated_2", ToolRejectedNotRegistered),
		rejectedCall("hallucinated_1", ToolRejectedNotRegistered),
	})

	snap := c.ToolSnapshot(GlobalScope())
	row := findOverflow(t, snap, "unregistered")
	if row.Tool != nil || row.Registered {
		t.Fatalf("the unregistered row carries no name: %+v", row)
	}
	// Different hallucinated names are different identities: one retry (the
	// repeated hallucinated_1) and three failure attempts, two identity-turns.
	if row.Counters.Attempts != 3 || row.Counters.Retries != 1 || row.Counters.FailureIdentityTurns != 2 || row.Counters.InvokedTurns != 1 {
		t.Fatalf("counters %+v", row.Counters)
	}
	if row.Rejections.NotRegistered != 3 {
		t.Fatalf("rejections %+v", row.Rejections)
	}
	for name := range c.tool.trackedTools {
		if strings.HasPrefix(name, "hallucinated") {
			t.Fatalf("a hallucinated name %q became a tracked key", name)
		}
	}
	data, _ := ToolJSON(snap, GlobalScope())
	if strings.Contains(string(data), "hallucinated") {
		t.Fatal("raw hallucinated names must never reach an export")
	}
}

func TestToolTaggedOverflowNeverCollidesWithARealToolName(t *testing.T) {
	c := newToolCollector()
	names := []string{"other_registered", "unregistered"}
	for i := 0; i < MaxTrackedTools; i++ {
		names = append(names, fmt.Sprintf("tool_%03d", i))
	}
	recordTurn(c, names, []ToolCallObservation{okCall("other_registered"), okCall("unregistered")})
	recordTurn(c, []string{"overflowing"}, []ToolCallObservation{okCall("overflowing")})

	snap := c.ToolSnapshot(GlobalScope())
	ordinary := findTool(t, snap, "other_registered")
	if ordinary.Overflow != "" || ordinary.Counters.Attempts != 1 {
		t.Fatalf("a real tool literally named other_registered stays an ordinary row: %+v", ordinary)
	}
	if findTool(t, snap, "unregistered").Counters.Attempts != 1 {
		t.Fatal("a real tool named unregistered stays an ordinary row")
	}
	if findOverflow(t, snap, "other_registered").Counters.Attempts != 1 {
		t.Fatal("the folded tool must land in the tagged overflow row")
	}
}

func TestToolSideEffectsAndPreparationAreIndependentOfExecutionOutcome(t *testing.T) {
	c := newToolCollector()
	confirmedButUnprepared := okCall("effect")
	confirmedButUnprepared.SideEffecting, confirmedButUnprepared.SideEffectOutcome = true, "confirmed"
	confirmedButUnprepared.Preparation = ToolPreparationFailed
	ambiguous := errCall("effect", "outcome_unknown", "ambiguous_outcome")
	ambiguous.SideEffecting, ambiguous.SideEffectOutcome = true, "unknown"
	notApplied := errCall("effect", "invalid_arguments", "invalid_arguments")
	notApplied.SideEffecting, notApplied.SideEffectOutcome = true, "not_applied"
	reduced := okCall("effect")
	reduced.SideEffecting, reduced.SideEffectOutcome, reduced.Preparation = true, "confirmed", ToolPreparationReduced

	recordTurn(c, []string{"effect"}, []ToolCallObservation{confirmedButUnprepared, confirmedButUnprepared, ambiguous, notApplied, reduced},
		failing(ToolRunGeneration)) // a later run failure must not erase confirmed effects

	row := findTool(t, c.ToolSnapshot(GlobalScope()), "effect")
	if row.SideEffects != (ToolSideEffects{Confirmed: 3, NotApplied: 1, Unknown: 1}) {
		t.Fatalf("side effects %+v", row.SideEffects)
	}
	if row.Counters.PreparationFailed != 2 || row.Counters.PreparationReduced != 1 {
		t.Fatalf("preparation counters %+v", row.Counters)
	}
	// A preparation failure is not an execution failure: both prep-failed calls
	// succeeded, and the second must not count as a retry/recovery.
	if row.Counters.Succeeded != 3 || row.Counters.Failed != 2 {
		t.Fatalf("execution outcomes %+v", row.Counters)
	}
	if row.Counters.RepeatsAfterPrepFailure < 1 {
		t.Fatalf("repeats after a preparation failure must be visible: %+v", row.Counters)
	}
	if row.Counters.FailureIdentityTurns != 1 {
		t.Fatalf("only the real execution failures open a failure identity-turn: %+v", row.Counters)
	}
}

func TestToolPreparationFailureAloneTriggersNoRecoveryAccounting(t *testing.T) {
	c := newToolCollector()
	unprepared := okCall("t")
	unprepared.Preparation = ToolPreparationFailed
	recordTurn(c, []string{"t"}, []ToolCallObservation{unprepared, okCall("t")})

	row := findTool(t, c.ToolSnapshot(GlobalScope()), "t")
	if row.Counters.Retries != 0 || row.Counters.RecoveredCalls != 0 || row.Counters.FailureIdentityTurns != 0 || row.Counters.RepeatsAfterPrepFailure != 1 {
		t.Fatalf("%+v", row.Counters)
	}
}

func TestToolUnrecognizedStatusCountsAsFailureWithDisclosure(t *testing.T) {
	c := newToolCollector()
	call := ToolCallObservation{Name: "t", Executed: true, Status: "weird"}
	recordTurn(c, []string{"t"}, []ToolCallObservation{call})

	row := findTool(t, c.ToolSnapshot(GlobalScope()), "t")
	if row.Counters.Failed != 1 || row.Counters.StatusUnrecognized != 1 || row.ErrorClasses.Other != 1 {
		t.Fatalf("%+v %+v", row.Counters, row.ErrorClasses)
	}
}

func TestToolFailedCancelledAndTimedOutTurnsRetainStatistics(t *testing.T) {
	c := newToolCollector()
	recordTurn(c, []string{"t"}, []ToolCallObservation{okCall("t")}, failing(ToolRunTimeout))
	recordTurn(c, []string{"t"}, []ToolCallObservation{okCall("t")}, failing(ToolRunCancelled), proactive())
	recordTurn(c, []string{"t"}, nil, failing(ToolRunGeneration))

	snap := c.ToolSnapshot(GlobalScope())
	if findTool(t, snap, "t").Counters.Attempts != 2 {
		t.Fatal("tool calls of failed turns must be retained")
	}
	var explicit, pro ToolTurnView
	for _, v := range snap.Turns {
		if v.Mode == "explicit" {
			explicit = v
		} else {
			pro = v
		}
	}
	if explicit.Turns != 2 || explicit.TurnsWithCalls != 1 || explicit.RunFailures != (ToolRunFailures{Timeout: 1, Generation: 1}) {
		t.Fatalf("explicit turns %+v", explicit)
	}
	if pro.Turns != 1 || pro.RunFailures.Cancelled != 1 {
		t.Fatalf("proactive turns %+v", pro)
	}
}

// ---- attribution and scope ----

func TestToolModelAndModeAttributionAndPerChatAggregation(t *testing.T) {
	c := newToolCollector()
	recordTurn(c, []string{"t"}, []ToolCallObservation{okCall("t")}, inChat(10), withModel("gm-a"))
	recordTurn(c, []string{"t"}, []ToolCallObservation{errCall("t", "e", "transient")}, inChat(10), withModel("gm-b"), proactive())
	recordTurn(c, []string{"t"}, []ToolCallObservation{okCall("t"), okCall("t")}, inChat(20), withModel("gm-a"))

	global := c.ToolSnapshot(GlobalScope())
	row := findTool(t, global, "t")
	if row.Counters.Attempts != 4 || row.Modes.Explicit.Attempts != 3 || row.Modes.Proactive.Attempts != 1 || row.Modes.Proactive.Unsuccessful != 1 {
		t.Fatalf("global row %+v %+v", row.Counters, row.Modes)
	}
	attribution := map[string]ToolModelView{}
	for _, m := range global.Models {
		attribution[*m.Model+"/"+m.Mode] = m
	}
	if got := attribution["gm-a/explicit"]; got.Turns != 2 || got.Attempts != 3 || got.Succeeded != 3 {
		t.Fatalf("gm-a/explicit %+v", got)
	}
	if got := attribution["gm-b/proactive"]; got.Turns != 1 || got.Unsuccessful != 1 {
		t.Fatalf("gm-b/proactive %+v", got)
	}

	chat10 := c.ToolSnapshot(ChatScope(10))
	if findTool(t, chat10, "t").Counters.Attempts != 2 || chat10.Models != nil {
		t.Fatalf("chat scope: per-chat totals, no model attribution: %+v", chat10.Models)
	}
	if got := findTool(t, c.ToolSnapshot(ChatScope(20)), "t").Counters.Attempts; got != 2 {
		t.Fatalf("chat 20 attempts %d", got)
	}
}

func TestToolZeroUseAndNeverAdvertisedTools(t *testing.T) {
	c := newToolCollector()
	c.SetToolCatalog([]string{"a", "b", "c"})
	snap := c.ToolSnapshot(GlobalScope())
	if len(snap.Tools) != 3 {
		t.Fatalf("registered tools appear before any usage: %d", len(snap.Tools))
	}
	for _, row := range snap.Tools {
		if !row.Registered || !row.NeverAdvertised || row.AdvertisedUnused || row.Counters.Attempts != 0 {
			t.Fatalf("zero-use row %+v", row)
		}
	}

	recordTurn(c, []string{"a"}, nil)
	snap = c.ToolSnapshot(GlobalScope())
	if a := findTool(t, snap, "a"); !a.AdvertisedUnused || a.NeverAdvertised {
		t.Fatalf("a was advertised but not used: %+v", a)
	}
	if b := findTool(t, snap, "b"); b.AdvertisedUnused || !b.NeverAdvertised {
		t.Fatalf("b was never advertised: %+v", b)
	}
	if snap.Catalog != (ToolCatalogInfo{Size: 3, Tracked: 3}) {
		t.Fatalf("catalog %+v", snap.Catalog)
	}
}

func TestToolCatalogBeyondTheCapIsBoundedAndDisclosed(t *testing.T) {
	c := newToolCollector()
	names := make([]string, 0, MaxTrackedTools+5)
	for i := 0; i < MaxTrackedTools+5; i++ {
		names = append(names, fmt.Sprintf("mcp_tool_%03d", i))
	}
	c.SetToolCatalog(names)

	snap := c.ToolSnapshot(GlobalScope())
	if len(snap.Tools) != MaxTrackedTools {
		t.Fatalf("zero-use rows must be bounded, got %d", len(snap.Tools))
	}
	if snap.Catalog.Size != MaxTrackedTools+5 || snap.Catalog.Omitted != 5 {
		t.Fatalf("omitted catalog entries must be disclosed: %+v", snap.Catalog)
	}
}

func TestToolChatAndModelCardinalityIsBounded(t *testing.T) {
	c := newToolCollector()
	for chat := int64(1); chat <= MaxTrackedChats+4; chat++ {
		recordTurn(c, []string{"t"}, []ToolCallObservation{okCall("t")}, inChat(chat), withModel(fmt.Sprintf("m%d", chat)))
	}
	recordTurn(c, []string{"t"}, nil, withModel(strings.Repeat("x", maxModelIDBytes+1)))

	snap := c.ToolSnapshot(GlobalScope())
	if snap.PerChatComplete || snap.ModelAttributionComplete || snap.OversizedIDFolds == 0 {
		t.Fatalf("overflow must be disclosed: %+v", snap)
	}
	if got := findTool(t, snap, "t").Counters.Attempts; got != MaxTrackedChats+4 {
		t.Fatalf("global totals stay exact under folding, got %d", got)
	}
	if len(c.tool.trackedChats) != MaxTrackedChats || len(c.tool.trackedModels) != MaxTrackedModels {
		t.Fatalf("tracked sets exceed their caps: %d chats %d models", len(c.tool.trackedChats), len(c.tool.trackedModels))
	}
	untracked := c.ToolSnapshot(ChatScope(MaxTrackedChats + 3))
	if untracked.UntrackedSelection == "" {
		t.Fatal("a chat folded into overflow must be disclosed, not shown as bare zeros")
	}
}

// ---- error codes ----

// overflowCodeLabel keys the folded bucket in errorCodeCounts; it cannot be a
// valid code because codes match [a-z0-9_]+.
const overflowCodeLabel = "<overflow>"

func errorCodeCounts(codes []ToolErrorCodeCount) map[string]uint64 {
	out := map[string]uint64{}
	for _, e := range codes {
		if e.Overflow {
			out[overflowCodeLabel] += e.Count

			continue
		}
		out[*e.Code] += e.Count
	}

	return out
}

// An admitted code literally named "other" must stay distinguishable from the
// folded bucket in the export.
func TestToolExportedOverflowErrorCodeNeverCollidesWithACodeNamedOther(t *testing.T) {
	c := newToolCollector()
	calls := []ToolCallObservation{errCall("t", "other", "permanent"), errCall("t", "other", "permanent"), errCall("t", "BAD CODE", "permanent")}
	recordTurn(c, []string{"t"}, calls)
	row := findTool(t, c.ToolSnapshot(GlobalScope()), "t")

	var named, folded *ToolErrorCodeCount
	for i := range row.ErrorCodes {
		if row.ErrorCodes[i].Overflow {
			folded = &row.ErrorCodes[i]
		} else {
			named = &row.ErrorCodes[i]
		}
	}
	if named == nil || named.Code == nil || *named.Code != "other" || named.Count != 2 {
		t.Fatalf("the admitted code named other must keep its own entry: %+v", row.ErrorCodes)
	}
	if folded == nil || folded.Code != nil || folded.Count != 1 {
		t.Fatalf("the folded bucket must be tagged, with no code name: %+v", row.ErrorCodes)
	}
	raw, err := json.Marshal(row.ErrorCodes)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"code":null,"overflow":true`) || !strings.Contains(string(raw), `"code":"other"`) {
		t.Fatalf("the JSON must distinguish both entries: %s", raw)
	}
}

func TestToolErrorCodeBounds(t *testing.T) {
	c := newToolCollector()
	var calls []ToolCallObservation
	for _, code := range []string{"UPPER", "has space", "", strings.Repeat("a", maxErrorCodeBytes+1), "valid_code"} {
		calls = append(calls, errCall("t", code, "permanent"))
	}
	recordTurn(c, []string{"t"}, calls)
	row := findTool(t, c.ToolSnapshot(GlobalScope()), "t")
	got := errorCodeCounts(row.ErrorCodes)
	if got["valid_code"] != 1 || got[overflowCodeLabel] != 4 || len(got) != 2 {
		t.Fatalf("invalid codes fold into other without becoming keys: %v", got)
	}
}

func TestToolErrorCodesPerToolAndDictionaryCaps(t *testing.T) {
	c := newToolCollector()
	var calls []ToolCallObservation
	for i := 0; i < MaxErrorCodesPerTool+5; i++ {
		calls = append(calls, errCall("t", fmt.Sprintf("code_%02d", i), "permanent"))
	}
	recordTurn(c, []string{"t"}, calls)
	row := findTool(t, c.ToolSnapshot(GlobalScope()), "t")
	if len(row.ErrorCodes) != MaxErrorCodesPerTool+1 { // admitted + "other"
		t.Fatalf("admitted codes per tool must be capped, got %d entries", len(row.ErrorCodes))
	}
	for _, e := range row.ErrorCodes {
		if e.Overflow && e.Count != 5 {
			t.Fatalf("the five late codes fold into other, got %d", e.Count)
		}
	}

	// The dictionary is dataset-wide: many tools with distinct codes exhaust it.
	d := newToolCollector()
	for i := 0; i < MaxTrackedErrorCodes+10; i++ {
		recordTurn(d, []string{"u"}, []ToolCallObservation{errCall(fmt.Sprintf("tool_%d", i), fmt.Sprintf("dict_%03d", i), "permanent")})
	}
	if len(d.tool.codeNames) != MaxTrackedErrorCodes {
		t.Fatalf("dictionary %d, want %d", len(d.tool.codeNames), MaxTrackedErrorCodes)
	}
}

// TestToolErrorCodeFoldingIsIdenticalAcrossProjectionsWhateverTheArrivalOrder
// feeds more than the per-tool cap of distinct codes in opposite orders
// through different chats and models. Admission is decided once per
// (tool, code), so the model and chat projections fold the same codes.
func TestToolErrorCodeFoldingIsIdenticalAcrossProjectionsWhateverTheArrivalOrder(t *testing.T) {
	c := newToolCollector()
	const codes = MaxErrorCodesPerTool + 4
	codeName := func(i int) string { return fmt.Sprintf("code_%02d", i) }
	for i := 0; i < codes; i++ { // chat A / model M1: ascending
		recordTurn(c, []string{"t"}, []ToolCallObservation{errCall("t", codeName(i), "permanent")}, inChat(1), withModel("M1"))
	}
	for i := codes - 1; i >= 0; i-- { // chat B / model M2: descending
		recordTurn(c, []string{"t"}, []ToolCallObservation{errCall("t", codeName(i), "permanent")}, inChat(2), withModel("M2"))
	}

	codeCounts := func(snap ToolSnapshot) map[string]uint64 {
		return errorCodeCounts(findTool(t, snap, "t").ErrorCodes)
	}
	global, chatA, chatB := codeCounts(c.ToolSnapshot(GlobalScope())), codeCounts(c.ToolSnapshot(ChatScope(1))), codeCounts(c.ToolSnapshot(ChatScope(2)))
	sum := map[string]uint64{}
	for _, m := range []map[string]uint64{chatA, chatB} {
		for code, n := range m {
			sum[code] += n
		}
	}
	if len(sum) != len(global) {
		t.Fatalf("projections fold differently: chats %v vs global %v", sum, global)
	}
	for code, n := range global {
		if sum[code] != n {
			t.Fatalf("code %q: chats sum to %d, global has %d", code, sum[code], n)
		}
	}
	// The first 16 arrivals were admitted; chat B's descending order must not
	// have admitted a different set.
	for i := 0; i < MaxErrorCodesPerTool; i++ {
		if global[codeName(i)] != 2 {
			t.Fatalf("%s was admitted first and is seen by both chats: %v", codeName(i), global)
		}
	}
	if global[overflowCodeLabel] != 2*4 {
		t.Fatalf("late codes fold into other in both projections: %v", global)
	}

	// The persisted payload passes restore validation (code sums agree).
	pair, payload := capturePayloadOf(t, c.ToolDataset())
	restored := newToolCollector()
	if err := restored.ToolDataset().Restore(toolFormatVersion, pair, payload); err != nil {
		t.Fatalf("restore validation must accept consistent folding: %v", err)
	}
}

// ---- resets and epochs ----

func TestToolResetDropsInFlightTurnsAndIsIndependent(t *testing.T) {
	c := newToolCollector()
	completeOperation(c, 1, "talk_low", "dm", "gm", ActionReply) // interaction data
	c.GroupRequest()
	recordTurn(c, []string{"t"}, []ToolCallObservation{okCall("t")})

	epoch := c.ToolEpoch() // captured before the turn runs
	generation := c.ResetToolUsage()
	if generation != 1 {
		t.Fatalf("generation %d", generation)
	}
	c.RecordToolTurn(ToolTurnObservation{Epoch: epoch, ChatID: 1, GenerationModel: "m", Advertised: []string{"t"}, Calls: []ToolCallObservation{okCall("t")}})

	snap := c.ToolSnapshot(GlobalScope())
	if len(snap.Tools) != 0 || snap.Period.Resets != 1 || snap.Turns[0].Turns != 0 {
		t.Fatalf("a turn that straddles a reset must contribute nothing: %+v", snap)
	}
	if got := c.InteractionSnapshot(GlobalScope(), InteractionFilters{}).Submission.Submitted; got != 1 {
		t.Fatalf("the tool reset must not touch interaction statistics, got %d", got)
	}
	if c.GeneralSnapshot().GroupRequests != 1 {
		t.Fatal("the tool reset must not touch general statistics")
	}

	// Independent in the other direction.
	recordTurn(c, []string{"t"}, []ToolCallObservation{okCall("t")})
	c.ResetInteraction()
	c.ResetGeneral()
	if findTool(t, c.ToolSnapshot(GlobalScope()), "t").Counters.Attempts != 1 {
		t.Fatal("interaction and general resets must not touch tool statistics")
	}
	if c.toolPersist.current.Generation != 1 || c.interactionPersist.current.Generation != 1 {
		t.Fatal("each dataset owns its own generation")
	}
}

func TestToolResetKeepsTheCatalogRows(t *testing.T) {
	c := newToolCollector()
	c.SetToolCatalog([]string{"a", "b"})
	recordTurn(c, []string{"a"}, []ToolCallObservation{okCall("a")})
	c.ResetToolUsage()

	snap := c.ToolSnapshot(GlobalScope())
	if len(snap.Tools) != 2 || findTool(t, snap, "a").Counters.Attempts != 0 {
		t.Fatalf("a reset keeps registered tools as zero-use rows: %+v", snap.Tools)
	}
}

func TestToolInvalidObservationsAreIgnored(t *testing.T) {
	c := newToolCollector()
	c.RecordToolTurn(ToolTurnObservation{Epoch: c.ToolEpoch(), Mode: toolModeCount})
	c.RecordToolTurn(ToolTurnObservation{Epoch: c.ToolEpoch(), RunFailure: toolRunFailureCount})
	if c.toolPersist.current.Revision != 0 {
		t.Fatal("invalid observations must not touch the dataset")
	}
}

func TestToolConcurrentCollectionSnapshotsAndResets(t *testing.T) {
	c := newToolCollector()
	var wg sync.WaitGroup
	for g := 0; g < 6; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 300; i++ {
				recordTurn(c, []string{"a", "b"}, []ToolCallObservation{okCall("a"), errCall("b", "e", "transient"), okCall("b")},
					inChat(int64(g)), withModel(fmt.Sprintf("m%d", g%3)))
			}
		}(g)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			snap := c.ToolSnapshot(GlobalScope())
			for _, row := range snap.Tools {
				if row.Counters.Succeeded+row.Counters.Failed+row.Counters.Rejected != row.Counters.Attempts {
					t.Errorf("inconsistent snapshot row %+v", row.Counters)

					return
				}
			}
			if i%10 == 9 {
				c.ResetToolUsage()
			}
		}
	}()
	wg.Wait()

	// Whatever happened, the dataset must satisfy its persisted invariants.
	pair, payload := capturePayloadOf(t, c.ToolDataset())
	if err := newToolCollector().ToolDataset().Restore(toolFormatVersion, pair, payload); err != nil {
		t.Fatalf("a concurrently collected dataset must restore cleanly: %v", err)
	}
}
