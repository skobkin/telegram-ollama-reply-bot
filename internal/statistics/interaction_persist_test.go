package statistics

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

var persistTestNow = time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)

func newPersistTestCollector() *Collector {
	return NewCollector(Config{Now: func() time.Time { return persistTestNow }, RubricVersion: 3})
}

// completeOperation records one fully terminated proactive operation.
func completeOperation(c *Collector, chat int64, profile, decisionModel, generationModel string, action InteractionAction) {
	epoch := c.BeginSubmission(chat)
	keys := ik(chat, profile, decisionModel)
	c.RecordInference(InferenceAttempt{Epoch: epoch, Keys: keys, Model: decisionModel, Latency: 700 * time.Millisecond, ErrClass: InferenceSuccess})
	outcome := OutcomeSelectedReact
	if action == ActionReply {
		outcome = OutcomeSelectedReply
	}
	c.RecordDecision(DecisionObservation{Epoch: epoch, Keys: keys, Outcome: outcome, Scores: scores(0.8), DecisionLatency: 900 * time.Millisecond})
	c.RecordExecutionDispatch(ExecutionDispatch{Epoch: epoch, Keys: keys, GenerationModel: generationModel, Action: action})
	c.RecordExecution(ExecutionObservation{
		Epoch: epoch, Keys: keys, GenerationModel: generationModel, Action: action,
		ReactionConfirmed: action == ActionReact, TextConfirmed: action == ActionReply, Duration: 2 * time.Second,
	})
}

// populatedCollector builds a collector with every dimension in play,
// including overflow keys, and NO pending operations.
func populatedCollector(t *testing.T) *Collector {
	t.Helper()
	c := newPersistTestCollector()
	for chat := int64(1); chat <= MaxTrackedChats+5; chat++ { // overflow chats
		for i, profile := range []string{"talk_low", "talk_high"} {
			completeOperation(c, chat, profile, fmt.Sprintf("dm%d", int(chat)%3), fmt.Sprintf("gm%d", i), ActionReply)
		}
	}
	for i := 0; i < MaxTrackedModels+3; i++ { // overflow models
		completeOperation(c, 1, "talk_low", fmt.Sprintf("many-%d", i), fmt.Sprintf("gen-%d", i), ActionReact)
	}
	for i := 0; i < MaxTrackedProfiles+2; i++ { // overflow profiles
		completeOperation(c, 2, fmt.Sprintf("profile_%d", i), "dm0", "gm0", ActionReact)
	}
	completeOperation(c, 3, "talk_low", strings.Repeat("x", maxModelIDBytes+1), "gm0", ActionReact) // oversized fold

	e := c.BeginSubmission(4)
	c.RecordPreflight(PreflightObservation{Epoch: e, ChatID: 4, Kind: PreflightEligibilityLost})
	e = c.BeginSubmission(4)
	c.RecordAcceptedCancelled(e, 4)
	e = c.BeginSubmission(4)
	c.RecordSubmitRejected(e, 4)
	e = c.BeginSubmission(4)
	c.RecordExecutionDispatch(ExecutionDispatch{Epoch: e, Keys: ik(4, "talk_low", "dm0"), GenerationModel: "gm0", Action: ActionReply})
	c.RecordExecution(ExecutionObservation{Epoch: e, Keys: ik(4, "talk_low", "dm0"), GenerationModel: "gm0", Action: ActionReply, Failure: ExecFailureTimeout, Duration: time.Second})
	c.RecordDecision(DecisionObservation{Epoch: e, Keys: ik(4, "talk_low", "dm0"), Outcome: OutcomeSelectedReply, Scores: scores(0.9), DecisionLatency: time.Second})

	return c
}

func snapshotJSON(t *testing.T, c *Collector, scope InteractionScope, filters InteractionFilters) string {
	t.Helper()
	data, err := json.Marshal(c.InteractionSnapshot(scope, filters))
	if err != nil {
		t.Fatal(err)
	}

	return string(data)
}

func capturePayload(t *testing.T, c *Collector) (PersistPair, []byte) {
	t.Helper()
	pair, encode, dirty := c.InteractionDataset().Capture()
	if !dirty {
		t.Fatal("expected a dirty dataset")
	}
	payload, err := encode()
	if err != nil {
		t.Fatal(err)
	}

	return pair, payload
}

func restoreInto(t *testing.T, pair PersistPair, payload []byte) *Collector {
	t.Helper()
	c := newPersistTestCollector()
	if err := c.InteractionDataset().Restore(interactionFormatVersion, pair, payload); err != nil {
		t.Fatalf("restore: %v", err)
	}

	return c
}

func TestInteractionRoundTripPreservesSnapshotsExactly(t *testing.T) {
	src := populatedCollector(t)
	src.ResetGeneral() // general statistics are never persisted; must not matter
	pair, payload := capturePayload(t, src)
	dst := restoreInto(t, pair, payload)

	views := []struct {
		name    string
		scope   InteractionScope
		filters InteractionFilters
	}{
		{"global", GlobalScope(), InteractionFilters{}},
		{"chat tracked", ChatScope(1), InteractionFilters{}},
		{"chat overflow-adjacent", ChatScope(MaxTrackedChats + 3), InteractionFilters{}},
		{"profile filter", GlobalScope(), InteractionFilters{Profile: "talk_low"}},
		{"model filter", GlobalScope(), InteractionFilters{DecisionModel: "dm0"}},
	}
	for _, v := range views {
		want, got := snapshotJSON(t, src, v.scope, v.filters), snapshotJSON(t, dst, v.scope, v.filters)
		if want != got {
			t.Fatalf("%s: snapshot differs after the round trip\nwant %s\n got %s", v.name, want, got)
		}
	}

	// The persisted pair and period survive.
	snap := dst.InteractionSnapshot(GlobalScope(), InteractionFilters{})
	if !snap.Period.StartedAt.Equal(persistTestNow) {
		t.Fatalf("observation period must survive restarts, got %v", snap.Period.StartedAt)
	}
	if dst.interactionPersist.current != pair || dst.interactionPersist.dirty() {
		t.Fatalf("restored tracker must be clean at the persisted pair: %+v", dst.interactionPersist)
	}
}

func TestInteractionRestoreKeepsOverflowBucketsAndFoldCountersUnchanged(t *testing.T) {
	src := populatedCollector(t)
	pair, payload := capturePayload(t, src)
	dst := restoreInto(t, pair, payload)

	before := src.interaction.oversizedIDFolds
	if before == 0 {
		t.Fatal("fixture must contain an oversized fold")
	}
	if dst.interaction.oversizedIDFolds != before {
		t.Fatalf("restoration must not count as new folds: %d != %d", dst.interaction.oversizedIDFolds, before)
	}

	var chatOverflow, profileOverflow, modelOverflow bool
	for key := range dst.interaction.decisionBuckets {
		chatOverflow = chatOverflow || key.chat.overflow
		profileOverflow = profileOverflow || key.profile.overflow
		modelOverflow = modelOverflow || key.model.overflow
	}
	if !chatOverflow || !profileOverflow || !modelOverflow {
		t.Fatalf("overflow buckets must be restored as overflow: chat=%v profile=%v model=%v", chatOverflow, profileOverflow, modelOverflow)
	}
	// An overflow tag is never a label: a profile literally named like an
	// overflow label stays an ordinary tracked profile.
	if len(dst.interaction.profiles) != MaxTrackedProfiles || len(dst.interaction.trackedChats) != MaxTrackedChats {
		t.Fatalf("tracked sets must be restored verbatim, got %d profiles %d chats", len(dst.interaction.profiles), len(dst.interaction.trackedChats))
	}
}

func TestInteractionRestoreInstallsGenerationAsEpoch(t *testing.T) {
	src := populatedCollector(t)
	src.ResetInteraction()
	src.ResetInteraction()
	completeOperation(src, 9, "talk_low", "m", "g", ActionReact)
	pair, payload := capturePayload(t, src)
	if pair.Generation != 2 {
		t.Fatalf("expected generation 2, got %+v", pair)
	}
	dst := restoreInto(t, pair, payload)

	epoch := dst.BeginSubmission(9)
	if epoch != 2 {
		t.Fatalf("the restored generation is the live epoch, got %d", epoch)
	}
	if got := dst.InteractionSnapshot(GlobalScope(), InteractionFilters{}).Period.Resets; got != 2 {
		t.Fatalf("resets must be restored, got %d", got)
	}
}

func TestInteractionInterruptedOperationsAreReconciled(t *testing.T) {
	c := populatedCollector(t)
	// Pending decision phases at the last flush, including a PROVISIONAL
	// submission (BeginSubmission without Submit resolution).
	c.BeginSubmission(5)
	c.BeginSubmission(5)
	e := c.BeginSubmission(6)
	// A dispatched-but-unfinished execution.
	c.RecordExecutionDispatch(ExecutionDispatch{Epoch: e, Keys: ik(6, "talk_low", "dm0"), GenerationModel: "gm0", Action: ActionReply})
	c.RecordExecutionDispatch(ExecutionDispatch{Epoch: e, Keys: ik(6, "talk_low", "dm0"), GenerationModel: "gm0", Action: ActionReact})

	before := c.InteractionSnapshot(GlobalScope(), InteractionFilters{})
	pair, payload := capturePayload(t, c)
	dst := restoreInto(t, pair, payload)
	snap := dst.InteractionSnapshot(GlobalScope(), InteractionFilters{})

	if snap.Submission.InterruptedByRestart != 3 {
		t.Fatalf("three pending submissions must be interrupted, got %d", snap.Submission.InterruptedByRestart)
	}
	if snap.Execution.Totals.TurnsInterrupted != 2 {
		t.Fatalf("two unfinished executions must be interrupted, got %d", snap.Execution.Totals.TurnsInterrupted)
	}
	// Nothing was invented: outcomes, effects and failures are unchanged.
	if snap.Decision.Totals.Outcomes != before.Decision.Totals.Outcomes ||
		snap.Execution.Totals.ReactionsConfirmed != before.Execution.Totals.ReactionsConfirmed ||
		snap.Execution.Totals.TextsConfirmed != before.Execution.Totals.TextsConfirmed ||
		snap.Execution.Totals.Failures != before.Execution.Totals.Failures {
		t.Fatal("reconciliation must not invent outcomes, effects or failures")
	}
	// The ledger closes: no permanently pending operations remain.
	sub := snap.Submission
	closed := sub.Accepted() - sub.AcceptedThenCancelled - snap.Preflight.Totals.Sum() - snap.Decision.Totals.Outcomes.Sum() - sub.InterruptedByRestart
	if closed != 0 {
		t.Fatalf("pending decision phases must be zero after reconciliation, got %d", closed)
	}
	chat5 := dst.InteractionSnapshot(ChatScope(5), InteractionFilters{})
	if chat5.Submission.InterruptedByRestart != 2 {
		t.Fatalf("per-chat interrupted must be attributed to the chat, got %d", chat5.Submission.InterruptedByRestart)
	}
	if !dst.interactionPersist.dirty() {
		t.Fatal("reconciliation must mark the restored dataset dirty so it is persisted")
	}
}

func TestInteractionRepeatedRestoreFlushCyclesDoNotDoubleCount(t *testing.T) {
	c := populatedCollector(t)
	c.BeginSubmission(60)
	e := c.BeginSubmission(60)
	c.RecordExecutionDispatch(ExecutionDispatch{Epoch: e, Keys: ik(60, "talk_low", "dm0"), GenerationModel: "gm0", Action: ActionReact})

	pair, payload := capturePayload(t, c)
	generation := restoreInto(t, pair, payload)
	var want string
	for cycle := 0; cycle < 4; cycle++ {
		// Flush the reconciled state and restart again.
		p2, encode, dirty := generation.InteractionDataset().Capture()
		if cycle == 0 && !dirty {
			t.Fatal("the first reconciled restore must be dirty")
		}
		if !dirty {
			// Nothing changed: the previous payload is still authoritative.
			continue
		}
		payload2, err := encode()
		if err != nil {
			t.Fatal(err)
		}
		generation = restoreInto(t, p2, payload2)
		got := snapshotJSON(t, generation, GlobalScope(), InteractionFilters{})
		if want == "" {
			want = got
		} else if got != want {
			t.Fatalf("cycle %d changed the snapshot: restore/flush/restart must be idempotent", cycle)
		}
		snap := generation.InteractionSnapshot(GlobalScope(), InteractionFilters{})
		if snap.Submission.InterruptedByRestart != 2 || snap.Execution.Totals.TurnsInterrupted != 1 {
			t.Fatalf("cycle %d: interrupted counts changed to %d/%d", cycle, snap.Submission.InterruptedByRestart, snap.Execution.Totals.TurnsInterrupted)
		}
		if generation.interactionPersist.dirty() {
			t.Fatalf("cycle %d: a clean restore must not be dirty", cycle)
		}
	}
}

func TestInteractionRestoredOperationsContinueNormally(t *testing.T) {
	src := populatedCollector(t)
	pair, payload := capturePayload(t, src)
	dst := restoreInto(t, pair, payload)

	completeOperation(dst, 1, "talk_low", "dm0", "gm0", ActionReply)
	snap := dst.InteractionSnapshot(GlobalScope(), InteractionFilters{})
	want := src.InteractionSnapshot(GlobalScope(), InteractionFilters{})
	if snap.Submission.Submitted != want.Submission.Submitted+1 {
		t.Fatal("collection must continue on top of the restored counters")
	}
}

func TestInteractionResetDuringFlushRejectsStaleWriteAndRestartShowsResetState(t *testing.T) {
	store, c := newFakeStore(), populatedCollector(t)
	p := testPersister(store, nil, c.InteractionDataset())
	ctx := context.Background()

	if err := p.FlushNow(ctx, InteractionDatasetName); err != nil {
		t.Fatal(err)
	}
	staleRow, _ := store.row(InteractionDatasetName)

	generation := c.ResetInteraction()
	if out := p.EnsureDurable(ctx, InteractionDatasetName, generation); !out.Durable {
		t.Fatalf("reset must become durable: %+v", out)
	}
	applied, err := store.SaveSnapshot(ctx, staleRow) // a delayed pre-reset write
	if err != nil || applied {
		t.Fatalf("the stale write must be rejected, applied=%v err=%v", applied, err)
	}

	restarted := newPersistTestCollector()
	p2 := testPersister(store, nil, restarted.InteractionDataset())
	if err := p2.Restore(ctx); err != nil {
		t.Fatal(err)
	}
	snap := restarted.InteractionSnapshot(GlobalScope(), InteractionFilters{})
	if snap.Submission.Submitted != 0 || snap.Period.Resets != 1 {
		t.Fatalf("a restart must show the reset state, got submitted=%d resets=%d", snap.Submission.Submitted, snap.Period.Resets)
	}
}

func TestInteractionResetIsDirtyEvenWhenRevisionEqualsSavedRevision(t *testing.T) {
	store, c := newFakeStore(), newPersistTestCollector()
	p := testPersister(store, nil, c.InteractionDataset())
	ctx := context.Background()

	c.BeginSubmission(1)
	if err := p.FlushNow(ctx, InteractionDatasetName); err != nil {
		t.Fatal(err)
	}
	savedRevision := c.interactionPersist.saved.Revision

	c.ResetInteraction()
	c.BeginSubmission(1) // revision after reset == the saved revision
	if c.interactionPersist.current.Revision != savedRevision {
		t.Fatalf("fixture must reproduce an equal revision, %d vs %d", c.interactionPersist.current.Revision, savedRevision)
	}
	if !c.interactionPersist.dirty() {
		t.Fatal("a newer generation with an equal revision must be dirty")
	}
}

func TestInteractionConcurrentCollectionPersistenceAndResets(t *testing.T) {
	store, c := newFakeStore(), newPersistTestCollector()
	p := testPersister(store, nil, c.InteractionDataset())
	ctx := context.Background()

	stop := make(chan struct{})
	var producers, background sync.WaitGroup
	for g := 0; g < 4; g++ {
		producers.Add(1)
		go func(g int) {
			defer producers.Done()
			for i := 0; i < 200; i++ {
				completeOperation(c, int64(g+1), "talk_low", "dm0", "gm0", ActionReact)
			}
		}(g)
	}
	background.Add(2)
	go func() { // flusher
		defer background.Done()
		for {
			select {
			case <-stop:
				return
			default:
				_ = p.FlushNow(ctx, InteractionDatasetName)
			}
		}
	}()
	go func() { // resetter and reader
		defer background.Done()
		for i := 0; i < 20; i++ {
			c.ResetInteraction()
			_ = c.InteractionSnapshot(GlobalScope(), InteractionFilters{})
		}
	}()

	producers.Wait()
	close(stop)
	background.Wait()
	if err := p.FlushNow(ctx, InteractionDatasetName); err != nil {
		t.Fatal(err)
	}

	// Whatever interleaving happened, the persisted row must restore and be
	// internally consistent.
	row, ok := store.row(InteractionDatasetName)
	if !ok {
		t.Fatal("expected a persisted row")
	}
	restored := newPersistTestCollector()
	if err := restored.InteractionDataset().Restore(row.FormatVersion, row.Pair, row.Payload); err != nil {
		t.Fatalf("a concurrently persisted snapshot must restore cleanly: %v", err)
	}
	// The live collector was flushed last, so the restored data must match
	// (the persistence disclosure legitimately differs).
	live, got := c.InteractionSnapshot(GlobalScope(), InteractionFilters{}), restored.InteractionSnapshot(GlobalScope(), InteractionFilters{})
	live.Persistence, got.Persistence = PersistenceInfo{}, PersistenceInfo{}
	liveJSON, _ := json.Marshal(live)
	gotJSON, _ := json.Marshal(got)
	if string(liveJSON) != string(gotJSON) {
		t.Fatal("the final flush must equal the live state")
	}
}

// ---- validation ----

func mutatePayload(t *testing.T, payload []byte, mutate func(p *interactionPayload)) []byte {
	t.Helper()
	var p interactionPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		t.Fatal(err)
	}
	mutate(&p)
	out, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}

	return out
}

func TestInteractionRestoreRejectsMalformedPayloads(t *testing.T) {
	src := populatedCollector(t)
	pair, payload := capturePayload(t, src)

	cases := map[string]func(p *interactionPayload){
		"global outcome total differs from buckets": func(p *interactionPayload) { p.Decision.Outcomes[OutcomeSelectedReply]++ },
		"global dispatched differs from buckets":    func(p *interactionPayload) { p.Execution.Dispatched[ActionReply]++ },
		"chat dictionary over the cap": func(p *interactionPayload) {
			for i := 0; i <= MaxTrackedChats; i++ {
				p.Chats = append(p.Chats, int64(10_000+i))
			}
		},
		"profile dictionary over the cap": func(p *interactionPayload) {
			for i := 0; i <= MaxTrackedProfiles; i++ {
				p.Profiles = append(p.Profiles, fmt.Sprintf("extra-%d", i))
			}
		},
		"duplicate chat entry":    func(p *interactionPayload) { p.Chats = append(p.Chats[:1], p.Chats[0]) },
		"dangling chat reference": func(p *interactionPayload) { p.DecisionBuckets[0].Chat = len(p.Chats) + 4 },
		"dangling model reference": func(p *interactionPayload) {
			p.ExecutionBuckets[0].GenerationModel = len(p.GenerationModels) + 1
		},
		"duplicate decision bucket": func(p *interactionPayload) {
			p.DecisionBuckets = append(p.DecisionBuckets, p.DecisionBuckets[0])
		},
		"histogram bin count mismatch": func(p *interactionPayload) {
			p.Decision.DecisionLatency = p.Decision.DecisionLatency[:3]
		},
		"decision latency total differs from outcomes": func(p *interactionPayload) {
			p.DecisionBuckets[0].DecisionLatency[0]++
			p.Decision.DecisionLatency[0]++
		},
		"failure classes differ from turns_failed": func(p *interactionPayload) {
			p.ExecutionBuckets[0].TurnsFailed++
			p.Execution.TurnsFailed++
		},
		"impossible ledger: decisions exceed submissions": func(p *interactionPayload) {
			for i := range p.SubmissionByChat {
				p.SubmissionByChat[i].Totals.Submitted = 0
			}
			p.Submission.Submitted = 0
		},
		"global submissions differ from per chat": func(p *interactionPayload) { p.Submission.Submitted += 5 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			bad := mutatePayload(t, payload, mutate)
			c := newPersistTestCollector()
			err := c.InteractionDataset().Restore(interactionFormatVersion, pair, bad)
			if !errors.Is(err, ErrMalformedSnapshot) {
				t.Fatalf("expected ErrMalformedSnapshot, got %v", err)
			}
			// A rejected restore leaves the live dataset untouched.
			if c.interaction.submission.Submitted != 0 {
				t.Fatal("a failed restore must not install partial state")
			}
		})
	}

	for name, raw := range map[string][]byte{
		"not json":      []byte("{"),
		"trailing data": append(append([]byte(nil), payload...), []byte(` {}`)...),
		"unknown field": []byte(`{"surprise":1}`),
		"empty":         nil,
		"wrong version": nil,
	} {
		t.Run(name, func(t *testing.T) {
			c := newPersistTestCollector()
			version := interactionFormatVersion
			if name == "wrong version" {
				version, raw = 0, payload
			}
			if err := c.InteractionDataset().Restore(version, pair, raw); !errors.Is(err, ErrMalformedSnapshot) {
				t.Fatalf("expected ErrMalformedSnapshot, got %v", err)
			}
		})
	}
}

func TestInteractionRestoreAcceptsEmptyDataset(t *testing.T) {
	c := newPersistTestCollector()
	c.BeginSubmission(1) // make it dirty so it can be captured
	c.ResetInteraction()
	c.BeginSubmission(1)
	pair, payload := capturePayload(t, c)
	if got := restoreInto(t, pair, payload).InteractionSnapshot(GlobalScope(), InteractionFilters{}).Submission.Submitted; got != 1 {
		t.Fatalf("got %d", got)
	}
}

func TestInteractionProfileOverflowIsBoundedAndFilterable(t *testing.T) {
	c := newPersistTestCollector()
	for i := 0; i < MaxTrackedProfiles+3; i++ {
		completeOperation(c, 1, fmt.Sprintf("p%d", i), "m", "g", ActionReact)
	}
	completeOperation(c, 1, strings.Repeat("p", maxProfileIDBytes+1), "m", "g", ActionReact)

	snap := c.InteractionSnapshot(GlobalScope(), InteractionFilters{})
	var overflow, tracked int
	for _, b := range snap.Decision.Buckets {
		if b.ProfileOverflow {
			overflow++
			if b.Profile != "" {
				t.Fatalf("an overflow profile must not carry a label, got %q", b.Profile)
			}
		} else {
			tracked++
		}
	}
	if tracked != MaxTrackedProfiles || overflow != 1 {
		t.Fatalf("tracked=%d overflow=%d", tracked, overflow)
	}
	if snap.OversizedIDFolds == 0 {
		t.Fatal("an oversized profile folds without truncation and is disclosed")
	}
	// A profile filter never matches an overflow bucket.
	filtered := c.InteractionSnapshot(GlobalScope(), InteractionFilters{Profile: "p0"})
	if len(filtered.Decision.Buckets) != 1 {
		t.Fatalf("expected exactly the p0 bucket, got %d", len(filtered.Decision.Buckets))
	}
	if filtered.FilteredTotalsExact {
		t.Fatal("a filtered view under overflow folding is not exact")
	}
}
