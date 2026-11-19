package statistics

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
)

// ik builds interaction keys for tests.
func ik(chat int64, profile, model string) InteractionKeys {
	return InteractionKeys{ChatID: chat, Profile: profile, DecisionModel: model, Mode: ModeLive}
}

// scores builds a full score vector.
func scores(base float64) *ScoreSet {
	return &ScoreSet{
		Base:             base,
		Relevance:        0.5,
		Contribution:     0.6,
		Intrusion:        0.1,
		ReactSuitability: 0.7,
		ReplySuitability: 0.4,
	}
}

func TestInteractionSubmissionLedgerAndInvariants(t *testing.T) {
	c := NewCollector(Config{})

	e1 := c.BeginSubmission(100) // accepted, decides normally
	e2 := c.BeginSubmission(101) // accepted, cancelled before callback
	e3 := c.BeginSubmission(104) // accepted, skipped in preflight
	_ = c.BeginSubmission(103)   // still undecided (pending): counted in accepted
	e4 := c.BeginSubmission(102) // closed-runner rejection
	c.RecordSubmitRejected(e4, 102)
	c.RecordAcceptedCancelled(e2, 101)

	// Every accepted operation terminates its decision phase as EXACTLY ONE
	// classification: a decision outcome (e1), accepted_then_cancelled (e2),
	// or a preflight kind (e3). The pending submission (103) keeps accepted
	// > terminated for now.
	c.RecordDecision(DecisionObservation{
		Epoch: e1, Keys: ik(100, "talk_low", "m1"),
		Outcome: OutcomeSelectedReply, Scores: scores(0.4), DecisionLatency: 1500 * time.Millisecond,
	})
	c.RecordPreflight(PreflightObservation{Epoch: e3, ChatID: 104, Kind: PreflightAccessDenied})

	snap := c.InteractionSnapshot(GlobalScope(), InteractionFilters{})
	if snap.Submission == nil {
		t.Fatalf("global scope must keep the submission section")
	}
	sub := snap.Submission
	if sub.Submitted != 5 {
		t.Fatalf("submitted = %d, want 5", sub.Submitted)
	}
	if sub.SubmitRejected != 1 {
		t.Fatalf("submit_rejected = %d, want 1", sub.SubmitRejected)
	}
	if sub.AcceptedThenCancelled != 1 {
		t.Fatalf("accepted_then_cancelled = %d, want 1", sub.AcceptedThenCancelled)
	}

	accepted := sub.Accepted()
	if accepted != 4 {
		t.Fatalf("accepted = %d, want 4", accepted)
	}
	terminated := snap.Preflight.Totals.Sum() + snap.Decision.Totals.Outcomes.Sum() + sub.AcceptedThenCancelled
	if terminated != 3 {
		t.Fatalf("terminated decision phases = %d, want 3 (outcome+cancelled+preflight; one op pending)", terminated)
	}
	if accepted <= terminated {
		t.Fatalf("pending op lost: accepted %d must exceed terminated %d", accepted, terminated)
	}

	// The pending operation terminates: the invariant closes exactly.
	c.RecordDecision(DecisionObservation{
		Epoch: 0, Keys: ik(103, "talk_low", "m1"), Outcome: OutcomeSelectedIgnoreBelow,
		Scores: scores(0.2), DecisionLatency: time.Second,
	})
	snap = c.InteractionSnapshot(GlobalScope(), InteractionFilters{})
	terminated = snap.Preflight.Totals.Sum() + snap.Decision.Totals.Outcomes.Sum() + snap.Submission.AcceptedThenCancelled
	if accepted != terminated {
		t.Fatalf("after full drain accepted must equal terminated; got %d vs %d", accepted, terminated)
	}
}

func TestInteractionResetEpochConsistency(t *testing.T) {
	c := NewCollector(Config{})

	// Old-epoch operation: submission captured, decision recorded.
	old := c.BeginSubmission(200)
	c.RecordDecision(DecisionObservation{
		Epoch: old, Keys: ik(200, "talk_low", "m1"), Outcome: OutcomeSelectedReact,
		Scores: scores(0.5), DecisionLatency: time.Second,
	})

	c.ResetInteraction()

	// Old-epoch records are dropped after the reset — including ones that
	// arrive late (simulating a task that was waiting for its slot or in
	// active inference when the reset happened).
	c.RecordDecision(DecisionObservation{
		Epoch: old, Keys: ik(200, "talk_low", "m1"), Outcome: OutcomeSelectedReact,
		Scores: scores(0.5), DecisionLatency: time.Second,
	})
	c.RecordPreflight(PreflightObservation{Epoch: old, ChatID: 200, Kind: PreflightAccessDenied})
	c.RecordExecutionDispatch(ExecutionDispatch{Epoch: old, Keys: ik(200, "talk_low", "m1"), Action: ActionReact})
	c.RecordExecution(ExecutionObservation{
		Epoch: old, Keys: ik(200, "talk_low", "m1"), Action: ActionReact,
		ReactionConfirmed: true, Duration: 2 * time.Second,
	})

	snap := c.InteractionSnapshot(GlobalScope(), InteractionFilters{})
	if snap.Period.Resets != 1 {
		t.Fatalf("period resets = %d, want 1", snap.Period.Resets)
	}
	if snap.Submission == nil || snap.Submission.Submitted != 0 {
		t.Fatalf("old-epoch submission must be gone after reset: %+v", snap.Submission)
	}
	if snap.Decision.Totals.Outcomes.Sum() != 0 {
		t.Fatalf("old-epoch outcomes must not survive reset: %+v", snap.Decision.Totals.Outcomes)
	}
	if snap.Execution.Totals.DispatchedReact != 0 || snap.Execution.Totals.ReactionsConfirmed != 0 {
		t.Fatalf("old-epoch execution records must not survive reset")
	}
	if len(snap.Decision.Buckets) != 0 || len(snap.Execution.Buckets) != 0 {
		t.Fatalf("reset must clear buckets, got %d decision / %d execution", len(snap.Decision.Buckets), len(snap.Execution.Buckets))
	}

	// New-epoch operation records normally.
	fresh := c.BeginSubmission(200)
	if fresh != old+1 {
		t.Fatalf("epoch after reset = %d, want %d", fresh, old+1)
	}
	c.RecordDecision(DecisionObservation{
		Epoch: fresh, Keys: ik(200, "talk_low", "m1"), Outcome: OutcomeSelectedReact,
		Scores: scores(0.5), DecisionLatency: time.Second,
	})

	snap = c.InteractionSnapshot(GlobalScope(), InteractionFilters{})
	if snap.Decision.Totals.Outcomes.SelectedReact != 1 {
		t.Fatalf("new-epoch outcome lost: %+v", snap.Decision.Totals.Outcomes)
	}
	if snap.Submission.Submitted != 1 {
		t.Fatalf("new-epoch submission = %d, want 1", snap.Submission.Submitted)
	}
}

func TestInteractionResetClearsObservationPeriod(t *testing.T) {
	base := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	now := base
	c := NewCollector(Config{Now: func() time.Time { return now }})

	first := c.InteractionSnapshot(GlobalScope(), InteractionFilters{}).Period.StartedAt
	if !first.Equal(base) {
		t.Fatalf("initial period start = %v, want %v", first, base)
	}

	now = base.Add(3 * time.Hour)
	c.ResetInteraction()

	second := c.InteractionSnapshot(GlobalScope(), InteractionFilters{}).Period.StartedAt
	if !second.Equal(now) {
		t.Fatalf("period start after reset = %v, want %v", second, now)
	}
}

func TestInteractionGeneralResetDoesNotTouchInteraction(t *testing.T) {
	c := NewCollector(Config{})

	epoch := c.BeginSubmission(300)
	c.RecordDecision(DecisionObservation{
		Epoch: epoch, Keys: ik(300, "talk_low", "m1"), Outcome: OutcomeSelectedReply,
		Scores: scores(0.4), DecisionLatency: time.Second,
	})

	c.ResetGeneral()

	snap := c.InteractionSnapshot(GlobalScope(), InteractionFilters{})
	if snap.Decision.Totals.Outcomes.SelectedReply != 1 {
		t.Fatalf("general reset must not touch interaction data")
	}
}

func TestInteractionHistogramBoundaries(t *testing.T) {
	h := newLatencyHistogram([]float64{100, 200})

	h.record(0)      // below first edge -> bin 0
	h.record(99.9)   // still bin 0
	h.record(100)    // exact edge -> upper bin 1
	h.record(150)    // inside bin 1
	h.record(200)    // exact last edge -> terminal bin 2
	h.record(10_000) // beyond -> terminal bin 2

	want := []uint64{2, 2, 2}
	for i, w := range want {
		if h.counts[i] != w {
			t.Fatalf("bin %d = %d, want %d (counts %v)", i, h.counts[i], w, h.counts)
		}
	}
	if h.total() != 6 {
		t.Fatalf("total = %d, want 6", h.total())
	}

	s := scoreHistogram{}
	s.record(0)     // clamps to bin 0
	s.record(-0.5)  // clamps to bin 0
	s.record(0.049) // bin 0
	s.record(0.05)  // bin 1
	s.record(0.999) // bin 19
	s.record(1.0)   // last bin inclusive
	s.record(1.5)   // clamps to bin 19

	if s.counts[0] != 3 {
		t.Fatalf("score bin 0 = %d, want 3", s.counts[0])
	}
	if s.counts[1] != 1 {
		t.Fatalf("score bin 1 = %d, want 1", s.counts[1])
	}
	if s.counts[19] != 3 {
		t.Fatalf("score bin 19 = %d, want 3", s.counts[19])
	}
}

func TestInteractionPercentileSemantics(t *testing.T) {
	t.Run("empty histogram reports null percentiles", func(t *testing.T) {
		h := newLatencyHistogram(latencyEdges)
		p := percentilesOf(h)
		if p.P50 != nil || p.P90 != nil || p.P99 != nil {
			t.Fatalf("empty histogram must yield null percentiles, got %+v", p)
		}
		data, err := json.Marshal(p)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if string(data) != `{"p50":null,"p90":null,"p99":null}` {
			t.Fatalf("null percentiles JSON = %s", data)
		}
	})

	t.Run("single observation interpolates inside its bin", func(t *testing.T) {
		h := newLatencyHistogram(latencyEdges)
		h.record(5000) // bin [5000,10000)

		p := percentilesOf(h)
		if p.P50 == nil || p.P50.ApproxMs == nil {
			t.Fatalf("p50 must be an approximation, got %+v", p.P50)
		}
		if *p.P50.ApproxMs < 5000 || *p.P50.ApproxMs > 10000 {
			t.Fatalf("p50 = %v outside its containing bin", *p.P50.ApproxMs)
		}
	})

	t.Run("terminal bin yields at_least lower bound", func(t *testing.T) {
		h := newLatencyHistogram(latencyEdges)
		h.record(500_000) // beyond the last edge -> open terminal bin

		p := percentilesOf(h)
		if p.P50 == nil || p.P50.AtLeastMs == nil || p.P50.ApproxMs != nil {
			t.Fatalf("terminal-bin p50 must be at_least_ms, got %+v", p.P50)
		}
		if *p.P50.AtLeastMs != 120000 {
			t.Fatalf("terminal lower bound = %v, want 120000", *p.P50.AtLeastMs)
		}
	})
}

func TestInteractionJSONNeverContainsInfOrNaN(t *testing.T) {
	c := NewCollector(Config{})

	epoch := c.BeginSubmission(400)
	c.RecordInference(InferenceAttempt{
		Epoch: epoch, Keys: ik(400, "talk_low", "m1"), Model: "m1",
		Latency: time.Hour, ErrClass: InferenceUnavailable,
	})
	c.RecordDecision(DecisionObservation{
		Epoch: epoch, Keys: ik(400, "talk_low", "m1"), Outcome: OutcomeBackendFailed,
		DecisionLatency: 24 * time.Hour,
	})
	c.RecordExecution(ExecutionObservation{
		Epoch: epoch, Keys: ik(400, "talk_low", "m1"), Action: ActionReply,
		Duration: 10 * time.Hour,
	})

	snap := c.InteractionSnapshot(GlobalScope(), InteractionFilters{})
	data, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	text := string(data)
	for _, bad := range []string{"Inf", "NaN", "+Inf"} {
		if strings.Contains(text, bad) {
			t.Fatalf("snapshot JSON contains %q; +Inf/NaN must never reach the encoder", bad)
		}
	}
}

func TestInteractionOverflowCardinality(t *testing.T) {
	c := NewCollector(Config{})

	// Fill the chat cap (32) with tracked chats, then push more chats into
	// the overflow key.
	for chat := int64(1); chat <= MaxTrackedChats+3; chat++ {
		epoch := c.BeginSubmission(chat)
		c.RecordDecision(DecisionObservation{
			Epoch: epoch, Keys: ik(chat, "talk_low", "m1"),
			Outcome: OutcomeSelectedIgnoreBelow, Scores: scores(0.2), DecisionLatency: 300 * time.Millisecond,
		})
	}

	snap := c.InteractionSnapshot(GlobalScope(), InteractionFilters{})
	if got := snap.Decision.Totals.Outcomes.SelectedIgnoreBelow; got != MaxTrackedChats+3 {
		t.Fatalf("global exact total = %d, want %d (totals are never bucket sums)", got, MaxTrackedChats+3)
	}

	var overflowBuckets int
	for _, b := range snap.Decision.Buckets {
		if b.ChatOverflow {
			overflowBuckets++
			if b.ChatID != nil {
				t.Fatalf("overflow bucket must carry a null chat_id")
			}
			if b.Counters.SelectedIgnoreBelow != 3 {
				t.Fatalf("overflow bucket count = %d, want 3", b.Counters.SelectedIgnoreBelow)
			}
		}
	}
	if overflowBuckets != 1 {
		t.Fatalf("overflow chat buckets = %d, want 1", overflowBuckets)
	}

	// Model dimension: 8 tracked, 9th folds into the model overflow key.
	c2 := NewCollector(Config{})
	for i := 0; i < MaxTrackedModels+1; i++ {
		model := "model-" + string(rune('a'+i))
		epoch := c2.BeginSubmission(1)
		c2.RecordDecision(DecisionObservation{
			Epoch: epoch, Keys: ik(1, "talk_low", model),
			Outcome: OutcomeSelectedIgnoreBelow, Scores: scores(0.2), DecisionLatency: time.Second,
		})
	}

	snap = c2.InteractionSnapshot(GlobalScope(), InteractionFilters{})
	if got := snap.Decision.Totals.Outcomes.SelectedIgnoreBelow; got != MaxTrackedModels+1 {
		t.Fatalf("model overflow: global total = %d, want %d", got, MaxTrackedModels+1)
	}
	var modelOverflow int
	for _, b := range snap.Decision.Buckets {
		if b.DecisionModelOverflow {
			modelOverflow++
			if b.DecisionModel != nil {
				t.Fatalf("model overflow bucket must carry a null decision_model")
			}
		}
	}
	if modelOverflow != 1 {
		t.Fatalf("model overflow buckets = %d, want 1", modelOverflow)
	}
}

func TestInteractionOversizedModelIDsOverflowWithoutTruncation(t *testing.T) {
	c := NewCollector(Config{})

	prefix := strings.Repeat("x", 120)
	longA := prefix + "-aaaaaaaa"
	longB := prefix + "-bbbbbbbb" // shares the 128-byte prefix, distinct ID
	if len(longA) <= maxModelIDBytes || len(longB) <= maxModelIDBytes {
		t.Fatalf("test IDs must exceed the byte cap")
	}

	for _, model := range []string{longA, longB} {
		epoch := c.BeginSubmission(1)
		c.RecordDecision(DecisionObservation{
			Epoch: epoch, Keys: ik(1, "talk_low", model),
			Outcome: OutcomeSelectedIgnoreBelow, Scores: scores(0.1), DecisionLatency: time.Second,
		})
	}

	snap := c.InteractionSnapshot(GlobalScope(), InteractionFilters{})
	if got := snap.Decision.Totals.Outcomes.SelectedIgnoreBelow; got != 2 {
		t.Fatalf("global total = %d, want 2", got)
	}
	if len(snap.Decision.Buckets) != 1 {
		t.Fatalf("oversized IDs must fold into ONE overflow bucket (no truncation-merge), got %d buckets", len(snap.Decision.Buckets))
	}
	b := snap.Decision.Buckets[0]
	if !b.DecisionModelOverflow || b.DecisionModel != nil {
		t.Fatalf("oversized bucket must be the tagged overflow key: %+v", b)
	}
}

func TestInteractionModelNamedOverflowIsOrdinary(t *testing.T) {
	c := NewCollector(Config{})

	epoch := c.BeginSubmission(1)
	c.RecordDecision(DecisionObservation{
		Epoch: epoch, Keys: ik(1, "talk_low", "overflow"),
		Outcome: OutcomeSelectedIgnoreBelow, Scores: scores(0.2), DecisionLatency: time.Second,
	})

	snap := c.InteractionSnapshot(GlobalScope(), InteractionFilters{})
	if len(snap.Decision.Buckets) != 1 {
		t.Fatalf("buckets = %d, want 1", len(snap.Decision.Buckets))
	}
	b := snap.Decision.Buckets[0]
	if b.DecisionModel == nil || *b.DecisionModel != "overflow" || b.DecisionModelOverflow {
		t.Fatalf("a model literally named \"overflow\" must render as a normal ID: %+v", b)
	}

	// And a filter by that name matches it (never the overflow key).
	filtered := c.InteractionSnapshot(GlobalScope(), InteractionFilters{DecisionModel: "overflow"})
	if len(filtered.Decision.Buckets) != 1 || filtered.Decision.Totals.Outcomes.SelectedIgnoreBelow != 1 {
		t.Fatalf("filter by the literal name \"overflow\" must match the real model")
	}
}

func TestInteractionFilteredTotalsHonestyUnderOverflow(t *testing.T) {
	c := NewCollector(Config{})

	for chat := int64(1); chat <= MaxTrackedChats; chat++ {
		epoch := c.BeginSubmission(chat)
		c.RecordDecision(DecisionObservation{
			Epoch: epoch, Keys: ik(chat, "talk_low", "m1"),
			Outcome: OutcomeSelectedIgnoreBelow, Scores: scores(0.2), DecisionLatency: time.Second,
		})
	}
	// One folded chat: overflow becomes active.
	epoch := c.BeginSubmission(10_000)
	c.RecordDecision(DecisionObservation{
		Epoch: epoch, Keys: ik(10_000, "talk_low", "m1"),
		Outcome: OutcomeSelectedIgnoreBelow, Scores: scores(0.2), DecisionLatency: time.Second,
	})

	global := c.InteractionSnapshot(GlobalScope(), InteractionFilters{})
	if !global.FilteredTotalsExact {
		t.Fatalf("unfiltered global view stays exact")
	}

	// Untracked chat selection: never bare zeros.
	chatView := c.InteractionSnapshot(ChatScope(99_999), InteractionFilters{})
	if chatView.FilteredTotalsExact {
		t.Fatalf("narrowed view under overflow must set filtered_totals_exact=false")
	}
	if chatView.UntrackedSelection == "" {
		t.Fatalf("untracked chat selection must set untracked_selection instead of bare zeros")
	}
	if len(chatView.Decision.Buckets) != 0 {
		t.Fatalf("untracked chat must match no buckets")
	}

	// Tracked chat selection: disclosure stays (overflow folding is active),
	// but the bucket is present.
	tracked := c.InteractionSnapshot(ChatScope(1), InteractionFilters{})
	if tracked.FilteredTotalsExact {
		t.Fatalf("any narrowed view under overflow must set filtered_totals_exact=false")
	}
	if tracked.UntrackedSelection != "" {
		t.Fatalf("tracked chat must not claim untracked selection")
	}
	if len(tracked.Decision.Buckets) != 1 || tracked.Decision.Totals.Outcomes.SelectedIgnoreBelow != 1 {
		t.Fatalf("tracked chat view must sum its own bucket")
	}

	// Untracked model filter: note instead of zeros.
	modelView := c.InteractionSnapshot(GlobalScope(), InteractionFilters{DecisionModel: "never-seen"})
	if modelView.UntrackedSelection == "" {
		t.Fatalf("untracked model filter must set untracked_selection")
	}
	if modelView.Submission != nil || modelView.Preflight != nil {
		t.Fatalf("chat-keyed sections must be omitted under model filters")
	}
}

func TestInteractionFilteredTotalsSumSelectedBuckets(t *testing.T) {
	c := NewCollector(Config{})

	record := func(chat int64, profile, model string, outcome InteractionOutcome) {
		epoch := c.BeginSubmission(chat)
		c.RecordDecision(DecisionObservation{
			Epoch: epoch, Keys: ik(chat, profile, model),
			Outcome: outcome, Scores: scores(0.3), DecisionLatency: time.Second,
		})
	}

	record(1, "talk_low", "m1", OutcomeSelectedReact)
	record(1, "talk_low", "m1", OutcomeSelectedReply)
	record(2, "mention", "m2", OutcomeSelectedIgnoreVeto)
	record(2, "mention", "m2", OutcomeSelectedIgnoreBelow)

	profileView := c.InteractionSnapshot(GlobalScope(), InteractionFilters{Profile: "talk_low"})
	if got := profileView.Decision.Totals.Outcomes.Sum(); got != 2 {
		t.Fatalf("profile-filtered outcomes = %d, want 2", got)
	}
	if profileView.Decision.Totals.Outcomes.SelectedReact != 1 || profileView.Decision.Totals.Outcomes.SelectedReply != 1 {
		t.Fatalf("profile-filtered outcome mix wrong: %+v", profileView.Decision.Totals.Outcomes)
	}
	if profileView.Submission != nil || profileView.Preflight != nil {
		t.Fatalf("chat-keyed sections must be omitted under profile filters")
	}

	modelView := c.InteractionSnapshot(GlobalScope(), InteractionFilters{DecisionModel: "m2"})
	if got := modelView.Decision.Totals.Outcomes.Sum(); got != 2 {
		t.Fatalf("model-filtered outcomes = %d, want 2", got)
	}

	global := c.InteractionSnapshot(GlobalScope(), InteractionFilters{})
	if got := global.Decision.Totals.Outcomes.Sum(); got != 4 {
		t.Fatalf("global outcomes = %d, want 4", got)
	}
	if len(global.Decision.Buckets) != 2 {
		t.Fatalf("global decision buckets = %d, want 2", len(global.Decision.Buckets))
	}

	// Chat scope keeps the chat-keyed sections.
	chatView := c.InteractionSnapshot(ChatScope(1), InteractionFilters{})
	if chatView.Submission == nil || chatView.Submission.Submitted != 2 {
		t.Fatalf("chat scope must keep submissions, got %+v", chatView.Submission)
	}
	if got := chatView.Decision.Totals.Outcomes.Sum(); got != 2 {
		t.Fatalf("chat-scoped outcomes = %d, want 2", got)
	}
}

func TestInteractionExecutionAccounting(t *testing.T) {
	c := NewCollector(Config{})

	epoch := c.BeginSubmission(1)
	keys := ik(1, "talk_low", "decision-m")
	gen := "generation-m"

	// Dispatch precedes completion.
	c.RecordExecutionDispatch(ExecutionDispatch{Epoch: epoch, Keys: keys, GenerationModel: gen, Action: ActionReact})
	c.RecordExecution(ExecutionObservation{
		Epoch: epoch, Keys: keys, GenerationModel: gen, Action: ActionReact,
		ReactionConfirmed: true, Duration: 1500 * time.Millisecond,
	})

	c.RecordExecutionDispatch(ExecutionDispatch{Epoch: epoch, Keys: keys, GenerationModel: gen, Action: ActionReply})
	// Partial success: reaction applied, generation then failed.
	c.RecordExecution(ExecutionObservation{
		Epoch: epoch, Keys: keys, GenerationModel: gen, Action: ActionReply,
		ReactionConfirmed: true, Failure: ExecFailureGeneration, Duration: 2 * time.Second,
	})

	c.RecordExecutionDispatch(ExecutionDispatch{Epoch: epoch, Keys: keys, GenerationModel: gen, Action: ActionReply})
	// Reply with a successful reaction AND another side effect: both flags.
	c.RecordExecution(ExecutionObservation{
		Epoch: epoch, Keys: keys, GenerationModel: gen, Action: ActionReply,
		ReactionConfirmed: true, TextConfirmed: true, OtherSideEffectsConfirmed: true,
		Duration: time.Second,
	})

	c.RecordExecutionDispatch(ExecutionDispatch{Epoch: epoch, Keys: keys, GenerationModel: gen, Action: ActionReply})
	// Successful turn with no tracked effect.
	c.RecordExecution(ExecutionObservation{
		Epoch: epoch, Keys: keys, GenerationModel: gen, Action: ActionReply,
		Duration: time.Second,
	})

	c.RecordExecutionDispatch(ExecutionDispatch{Epoch: epoch, Keys: keys, GenerationModel: gen, Action: ActionReply})
	// Failed turn with no effect, timeout class.
	c.RecordExecution(ExecutionObservation{
		Epoch: epoch, Keys: keys, GenerationModel: gen, Action: ActionReply,
		Failure: ExecFailureTimeout, Duration: 90 * time.Second,
	})

	snap := c.InteractionSnapshot(GlobalScope(), InteractionFilters{})
	tot := snap.Execution.Totals

	if tot.DispatchedReact != 1 || tot.DispatchedReply != 4 {
		t.Fatalf("dispatched = react %d / reply %d, want 1/4", tot.DispatchedReact, tot.DispatchedReply)
	}
	if tot.ReactionsConfirmed != 3 {
		t.Fatalf("reactions_confirmed = %d, want 3 (counted regardless of completion)", tot.ReactionsConfirmed)
	}
	if tot.TextsConfirmed != 1 {
		t.Fatalf("texts_confirmed = %d, want 1", tot.TextsConfirmed)
	}
	if tot.OtherSideEffects != 1 {
		t.Fatalf("other_side_effects = %d, want 1 (counts turns, not tool calls)", tot.OtherSideEffects)
	}
	if tot.TurnsWithEffect != 3 {
		t.Fatalf("turns_with_effect = %d, want 3", tot.TurnsWithEffect)
	}
	if tot.TurnsWithoutEffect != 1 {
		t.Fatalf("turns_without_effect = %d, want 1 (successful and effect-free only)", tot.TurnsWithoutEffect)
	}
	if tot.TurnsFailed != 2 {
		t.Fatalf("turns_failed = %d, want 2", tot.TurnsFailed)
	}
	if tot.Failures.Generation != 1 || tot.Failures.Timeout != 1 {
		t.Fatalf("failure classes wrong: %+v", tot.Failures)
	}
	if tot.Failures.Sum() != 2 {
		t.Fatalf("failure sum = %d, want 2", tot.Failures.Sum())
	}

	// selected (2 react? no — decision side) vs dispatched invariant is a
	// collection-point concern; here dispatched ≥ completed holds: 5
	// dispatches, 5 completions.
	if tot.DispatchedReact+tot.DispatchedReply != 5 {
		t.Fatalf("dispatched total = %d, want 5", tot.DispatchedReact+tot.DispatchedReply)
	}

	// Execution totals keep duration + percentiles.
	if tot.Duration.OpenEnded != true || len(tot.Duration.Counts) != len(executionLatencyEdges)+1 {
		t.Fatalf("execution duration histogram shape wrong: %+v", tot.Duration)
	}

	// Bucket carries BOTH effective model identifiers.
	if len(snap.Execution.Buckets) != 1 {
		t.Fatalf("execution buckets = %d, want 1", len(snap.Execution.Buckets))
	}
	b := snap.Execution.Buckets[0]
	if b.GenerationModel == nil || *b.GenerationModel != gen {
		t.Fatalf("generation model must record the effective ID, got %+v", b.GenerationModel)
	}
	if b.DecisionModel == nil || *b.DecisionModel != "decision-m" {
		t.Fatalf("decision model must be retained in execution buckets")
	}

	// Model filter matches the bucket by decision model.
	byDecision := c.InteractionSnapshot(GlobalScope(), InteractionFilters{DecisionModel: "decision-m"})
	if got := byDecision.Execution.Totals.DispatchedReply; got != 4 {
		t.Fatalf("execution filterable by decision model: dispatched_reply = %d, want 4", got)
	}

	// Filtered execution totals sum the selected buckets only.
	byProfile := c.InteractionSnapshot(GlobalScope(), InteractionFilters{Profile: "mention"})
	if byProfile.Execution.Totals.DispatchedReact+byProfile.Execution.Totals.DispatchedReply != 0 {
		t.Fatalf("non-matching profile must select nothing")
	}
}

func TestInteractionSnapshotImmutability(t *testing.T) {
	c := NewCollector(Config{})

	epoch := c.BeginSubmission(1)
	c.RecordDecision(DecisionObservation{
		Epoch: epoch, Keys: ik(1, "talk_low", "m1"),
		Outcome: OutcomeSelectedReact, Scores: scores(0.5), DecisionLatency: time.Second,
	})

	first := c.InteractionSnapshot(GlobalScope(), InteractionFilters{})

	// More activity after the snapshot…
	nextEpoch := c.BeginSubmission(2)
	c.RecordDecision(DecisionObservation{
		Epoch: nextEpoch, Keys: ik(2, "talk_low", "m1"),
		Outcome: OutcomeSelectedReply, Scores: scores(0.5), DecisionLatency: time.Second,
	})
	// …and mutation of the first snapshot's slices.
	if len(first.Decision.Buckets) > 0 {
		first.Decision.Buckets[0].Counters.SelectedReact = 999
		first.Decision.Buckets[0].Histograms.Base.Counts[0] = 999
	}

	second := c.InteractionSnapshot(GlobalScope(), InteractionFilters{})
	if first.Decision.Totals.Outcomes.Sum() != 1 {
		t.Fatalf("snapshot must be immutable after later recordings")
	}
	if second.Decision.Totals.Outcomes.Sum() != 2 {
		t.Fatalf("second snapshot must see new activity, got %d", second.Decision.Totals.Outcomes.Sum())
	}
	for _, b := range second.Decision.Buckets {
		if b.Counters.SelectedReact == 999 || b.Histograms.Base.Counts[0] == 999 {
			t.Fatalf("mutating one snapshot's slices leaked into collector state")
		}
	}
}

func TestInteractionConcurrentHammer(t *testing.T) {
	c := NewCollector(Config{})

	const workers = 12
	var wg sync.WaitGroup
	var readerWg sync.WaitGroup
	stop := make(chan struct{})

	// Concurrent snapshot readers must never observe an inconsistent ledger.
	readerWg.Add(1)
	go func() {
		defer readerWg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			snap := c.InteractionSnapshot(GlobalScope(), InteractionFilters{})
			sub := snap.Submission
			if sub == nil {
				continue
			}
			accepted := sub.Accepted()
			terminated := snap.Decision.Totals.Outcomes.Sum() + sub.AcceptedThenCancelled
			if accepted < terminated {
				t.Errorf("reader observed accepted %d < terminated %d", accepted, terminated)
			}
		}
	}()

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			chat := int64(w + 1) // within the tracked-chat cap
			epoch := c.BeginSubmission(chat)
			c.RecordInference(InferenceAttempt{
				Epoch: epoch, Keys: ik(chat, "talk_low", "m1"), Model: "m1",
				Latency: time.Second, ErrClass: InferenceSuccess,
			})
			c.RecordDecision(DecisionObservation{
				Epoch: epoch, Keys: ik(chat, "talk_low", "m1"),
				Outcome: OutcomeSelectedReply, Scores: scores(0.4), DecisionLatency: time.Second,
			})
			c.RecordExecutionDispatch(ExecutionDispatch{Epoch: epoch, Keys: ik(chat, "talk_low", "m1"), GenerationModel: "gen", Action: ActionReply})
			c.RecordExecution(ExecutionObservation{
				Epoch: epoch, Keys: ik(chat, "talk_low", "m1"), GenerationModel: "gen", Action: ActionReply,
				TextConfirmed: true, Duration: time.Second,
			})
		}(w)
	}

	wg.Wait()
	close(stop) // release the reader only after all writers finished
	readerWg.Wait()

	snap := c.InteractionSnapshot(GlobalScope(), InteractionFilters{})
	if snap.Submission.Submitted != workers {
		t.Fatalf("submitted = %d, want %d", snap.Submission.Submitted, workers)
	}
	if snap.Submission.Accepted() != workers {
		t.Fatalf("accepted = %d, want %d", snap.Submission.Accepted(), workers)
	}
	if snap.Decision.Totals.Outcomes.Sum() != workers {
		t.Fatalf("outcomes = %d, want %d", snap.Decision.Totals.Outcomes.Sum(), workers)
	}
	if snap.Decision.Totals.EvaluatedAttempts != workers {
		t.Fatalf("evaluated_attempts = %d, want %d", snap.Decision.Totals.EvaluatedAttempts, workers)
	}
	if snap.Execution.Totals.DispatchedReply != workers || snap.Execution.Totals.TextsConfirmed != workers {
		t.Fatalf("execution ledger mismatch: %+v", snap.Execution.Totals.ExecutionCounters)
	}

	// Invariants after drain.
	terminated := snap.Decision.Totals.Outcomes.Sum() + snap.Submission.AcceptedThenCancelled
	if snap.Submission.Accepted() != terminated {
		t.Fatalf("drained invariant broken: accepted %d != terminated %d", snap.Submission.Accepted(), terminated)
	}
}

func TestInteractionInferenceAttemptAccounting(t *testing.T) {
	c := NewCollector(Config{})

	epoch := c.BeginSubmission(1)
	keys := ik(1, "talk_low", "m1")

	c.RecordInference(InferenceAttempt{Epoch: epoch, Keys: keys, Model: "m1", Latency: time.Second, ErrClass: InferenceSuccess})
	c.RecordInference(InferenceAttempt{Epoch: epoch, Keys: keys, Model: "m1", Latency: 50 * time.Millisecond, ErrClass: InferencePretransportRejected})
	c.RecordInference(InferenceAttempt{Epoch: epoch, Keys: keys, Model: "m1", Latency: 3 * time.Second, ErrClass: InferenceAuth})
	c.RecordInference(InferenceAttempt{Epoch: epoch, Keys: keys, Model: "m1", Latency: time.Second, ErrClass: InferenceTimeout})

	// An out-of-range class is ignored, not counted.
	c.RecordInference(InferenceAttempt{Epoch: epoch, Keys: keys, Model: "m1", ErrClass: InferenceErrClass(99)})
	// An old epoch is dropped.
	c.RecordInference(InferenceAttempt{Epoch: epoch - 1, Keys: keys, Model: "m1", ErrClass: InferenceUnavailable})

	snap := c.InteractionSnapshot(GlobalScope(), InteractionFilters{})
	tot := snap.Decision.Totals
	if tot.EvaluatedAttempts != 4 {
		t.Fatalf("evaluated_attempts = %d, want 4", tot.EvaluatedAttempts)
	}
	if tot.PretransportRejected != 1 {
		t.Fatalf("pretransport_rejected = %d, want 1", tot.PretransportRejected)
	}
	if tot.InferenceErrors.Auth != 1 || tot.InferenceErrors.Timeout != 1 {
		t.Fatalf("error classes wrong: %+v", tot.InferenceErrors)
	}
	if tot.InferenceErrors.Unavailable != 0 {
		t.Fatalf("old-epoch record must be dropped")
	}

	// The bucket sums match the totals (single bucket here).
	if len(snap.Decision.Buckets) != 1 {
		t.Fatalf("buckets = %d, want 1", len(snap.Decision.Buckets))
	}
	b := snap.Decision.Buckets[0]
	for _, v := range b.Histograms.Base.Counts {
		if v != 0 {
			t.Fatalf("inference-only bucket must carry no scores")
		}
	}

	// The decision outcome records scores; the histogram fills up.
	c.RecordDecision(DecisionObservation{
		Epoch: epoch, Keys: keys, Outcome: OutcomeSelectedReact,
		Scores: scores(0.33), DecisionLatency: 1200 * time.Millisecond,
	})
	snap = c.InteractionSnapshot(GlobalScope(), InteractionFilters{})
	base := snap.Decision.Totals.ScoreHistograms.Base
	var baseTotal uint64
	for _, v := range base.Counts {
		baseTotal += v
	}
	if baseTotal != 1 {
		t.Fatalf("base score histogram total = %d, want 1", baseTotal)
	}
	// 0.33 -> bin 6 ([0.30,0.35)).
	if base.Counts[6] != 1 {
		t.Fatalf("base score 0.33 must land in bin 6, counts %v", base.Counts)
	}
	// Decision latency 1200ms -> bin 4 = [1000,2500).
	if snap.Decision.Totals.Latency.Decision.Counts[4] != 1 {
		t.Fatalf("decision latency 1200ms must land in the [1000,2500) bin, counts %v", snap.Decision.Totals.Latency.Decision.Counts)
	}
}

func TestInteractionPreflightAccounting(t *testing.T) {
	c := NewCollector(Config{})

	epoch := c.BeginSubmission(1)
	c.RecordPreflight(PreflightObservation{Epoch: epoch, ChatID: 1, Kind: PreflightAccessDenied})
	c.RecordPreflight(PreflightObservation{Epoch: epoch, ChatID: 1, Kind: PreflightAccessDenied})
	c.RecordPreflight(PreflightObservation{Epoch: epoch, ChatID: 2, Kind: PreflightEligibilityLost})
	// Unknown kind and old epoch: ignored.
	c.RecordPreflight(PreflightObservation{Epoch: epoch, ChatID: 1, Kind: PreflightKind(42)})
	c.RecordPreflight(PreflightObservation{Epoch: epoch - 1, ChatID: 1, Kind: PreflightAccessDenied})

	snap := c.InteractionSnapshot(GlobalScope(), InteractionFilters{})
	if snap.Preflight == nil {
		t.Fatalf("global scope must keep the preflight section")
	}
	if snap.Preflight.Totals.AccessDenied != 2 || snap.Preflight.Totals.EligibilityLost != 1 {
		t.Fatalf("preflight totals wrong: %+v", snap.Preflight.Totals)
	}
	if len(snap.Preflight.Chats) != 2 {
		t.Fatalf("preflight chats = %d, want 2", len(snap.Preflight.Chats))
	}
	// Sorted ascending by chat ID.
	if snap.Preflight.Chats[0].ChatID == nil || *snap.Preflight.Chats[0].ChatID != 1 {
		t.Fatalf("preflight chats must be sorted: %+v", snap.Preflight.Chats)
	}

	chatView := c.InteractionSnapshot(ChatScope(2), InteractionFilters{})
	if chatView.Preflight.Totals.EligibilityLost != 1 || chatView.Preflight.Totals.AccessDenied != 0 {
		t.Fatalf("chat-scoped preflight wrong: %+v", chatView.Preflight.Totals)
	}
}

func TestInteractionInvalidObservationsAreIgnored(t *testing.T) {
	c := NewCollector(Config{})

	epoch := c.BeginSubmission(1)
	keys := ik(1, "talk_low", "m1")

	c.RecordDecision(DecisionObservation{Epoch: epoch, Keys: keys, Outcome: InteractionOutcome(99)})
	c.RecordExecutionDispatch(ExecutionDispatch{Epoch: epoch, Keys: keys, Action: InteractionAction(77)})
	c.RecordExecution(ExecutionObservation{Epoch: epoch, Keys: keys, Action: ActionReply, Failure: ExecFailure(88)})

	snap := c.InteractionSnapshot(GlobalScope(), InteractionFilters{})
	if snap.Decision.Totals.Outcomes.Sum() != 0 {
		t.Fatalf("out-of-range outcome must be ignored")
	}
	if snap.Execution.Totals.DispatchedReact+snap.Execution.Totals.DispatchedReply != 0 {
		t.Fatalf("out-of-range action must be ignored")
	}
	if snap.Execution.Totals.TurnsFailed != 0 || snap.Execution.Totals.Failures.Sum() != 0 {
		t.Fatalf("out-of-range failure class must be ignored")
	}
}
