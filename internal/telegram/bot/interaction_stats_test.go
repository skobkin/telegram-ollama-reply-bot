package bot

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"telegram-ollama-reply-bot/internal/adminconfig"
	"telegram-ollama-reply-bot/internal/interactivity"
	"telegram-ollama-reply-bot/internal/llm"
	"telegram-ollama-reply-bot/internal/statistics"
	"telegram-ollama-reply-bot/internal/systemone"
)

// waitForInteraction polls the collector's interaction view until the
// condition over the snapshot holds, returning the satisfying snapshot.
func waitForInteraction(t *testing.T, stats *statistics.Collector, condition func(statistics.InteractionSnapshot) bool) statistics.InteractionSnapshot {
	t.Helper()

	var last statistics.InteractionSnapshot
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		last = stats.InteractionSnapshot(statistics.GlobalScope(), statistics.InteractionFilters{})
		if condition(last) {
			return last
		}
		time.Sleep(10 * time.Millisecond)
	}
	last = stats.InteractionSnapshot(statistics.GlobalScope(), statistics.InteractionFilters{})
	t.Fatalf("interaction condition not met within timeout; last snapshot: %+v", last)

	return last
}

// assertLedgerInvariants checks the decision-phase ledger invariant of a
// snapshot: accepted is never below the terminated decision phases.
func assertLedgerInvariants(t *testing.T, snap statistics.InteractionSnapshot) {
	t.Helper()

	if snap.Submission == nil {
		t.Fatalf("global scope must keep the submission section")
	}
	accepted := snap.Submission.Accepted()
	terminated := snap.Preflight.Totals.Sum() + snap.Decision.Totals.Outcomes.Sum() + snap.Submission.AcceptedThenCancelled
	if accepted < terminated {
		t.Fatalf("invariant violated: accepted %d < terminated %d (snapshot %+v)", accepted, terminated, snap)
	}
}

// outcomeCounted reports whether the totals carry exactly one of the given
// outcome.
func outcomeCounted(totals statistics.InteractionOutcomeTotals, want statistics.InteractionOutcome) bool {
	fields := map[statistics.InteractionOutcome]uint64{
		statistics.OutcomeNoopProfileDisabled: totals.NoopProfileDisabled,
		statistics.OutcomeUnconfigured:        totals.Unconfigured,
		statistics.OutcomePreparationFailed:   totals.PreparationFailed,
		statistics.OutcomeBackendTimeout:      totals.BackendTimeout,
		statistics.OutcomeBackendCancelled:    totals.BackendCancelled,
		statistics.OutcomeBackendAuth:         totals.BackendAuth,
		statistics.OutcomeBackendFailed:       totals.BackendFailed,
		statistics.OutcomeInvalidResponse:     totals.InvalidResponse,
		statistics.OutcomeEvidenceInvalid:     totals.EvidenceInvalid,
		statistics.OutcomeSelectedIgnoreVeto:  totals.SelectedIgnoreVeto,
		statistics.OutcomeSelectedIgnoreBelow: totals.SelectedIgnoreBelow,
		statistics.OutcomeSelectedReact:       totals.SelectedReact,
		statistics.OutcomeSelectedReply:       totals.SelectedReply,
	}

	return fields[want] == 1 && totals.Sum() == 1
}

// inferenceClassCount reads one class counter from the totals.
func inferenceClassCount(totals statistics.InferenceErrorClassTotals, class statistics.InferenceErrClass) uint64 {
	fields := map[statistics.InferenceErrClass]uint64{
		statistics.InferencePretransportRejected: totals.PretransportRejected,
		statistics.InferenceNotConfigured:        totals.NotConfigured,
		statistics.InferenceTimeout:              totals.Timeout,
		statistics.InferenceCancelled:            totals.Cancelled,
		statistics.InferenceAuth:                 totals.Auth,
		statistics.InferenceUnavailable:          totals.Unavailable,
		statistics.InferenceInvalidResponse:      totals.InvalidResponse,
	}

	return fields[class]
}

func histogramTotal(h statistics.HistogramSnapshot) uint64 {
	var total uint64
	for _, v := range h.Counts {
		total += v
	}

	return total
}

// classifyCase scripts one decision end-to-end through the real wiring.
type classifyCase struct {
	name        string
	profile     adminconfig.InteractionProfile
	answers     map[string]systemone.Answer
	err         error
	wantOutcome statistics.InteractionOutcome
	wantClass   statistics.InferenceErrClass
	wantScores  bool
}

func TestInteractionDecisionOutcomeClassification(t *testing.T) {
	cases := []classifyCase{
		{
			name:        "below thresholds selects ignore-below with scores",
			profile:     adminconfig.ProfileTalkLow,
			answers:     decisionAnswers(0.05, 0.0, 0.0),
			wantOutcome: statistics.OutcomeSelectedIgnoreBelow,
			wantClass:   statistics.InferenceSuccess,
			wantScores:  true,
		},
		{
			name:        "intrusion veto selects ignore-veto with scores",
			profile:     adminconfig.ProfileTalkLow,
			answers:     decisionAnswers(0.95, 1.0, 1.0),
			wantOutcome: statistics.OutcomeSelectedIgnoreVeto,
			wantClass:   statistics.InferenceSuccess,
			wantScores:  true,
		},
		{
			name:        "reply gate selects reply with scores",
			profile:     adminconfig.ProfileTalkLow,
			answers:     decisionAnswers(0.05, 0.0, 1.0),
			wantOutcome: statistics.OutcomeSelectedReply,
			wantClass:   statistics.InferenceSuccess,
			wantScores:  true,
		},
		{
			name:        "react gate selects react with scores",
			profile:     adminconfig.ProfileReactionsHigh,
			answers:     decisionAnswers(0.1, 1.0, 0.0),
			wantOutcome: statistics.OutcomeSelectedReact,
			wantClass:   statistics.InferenceSuccess,
			wantScores:  true,
		},
		{
			// Chain regression: the client wraps transport timeouts so the
			// context sentinel survives into the engine's %w:%w wrapping;
			// the collection point must see DeadlineExceeded through it.
			name:        "context deadline classifies backend timeout",
			profile:     adminconfig.ProfileTalkLow,
			err:         context.DeadlineExceeded,
			wantOutcome: statistics.OutcomeBackendTimeout,
			wantClass:   statistics.InferenceTimeout,
		},
		{
			name:        "cancellation classifies backend cancelled",
			profile:     adminconfig.ProfileTalkLow,
			err:         context.Canceled,
			wantOutcome: statistics.OutcomeBackendCancelled,
			wantClass:   statistics.InferenceCancelled,
		},
		{
			name:        "auth sentinel classifies backend auth",
			profile:     adminconfig.ProfileTalkLow,
			err:         fmt.Errorf("%w: http status 401", systemone.ErrAuth),
			wantOutcome: statistics.OutcomeBackendAuth,
			wantClass:   statistics.InferenceAuth,
		},
		{
			// Transport failures and non-2xx statuses share one client
			// sentinel: one class, never split by parsing error strings.
			name:        "unavailable sentinel classifies backend failed",
			profile:     adminconfig.ProfileTalkLow,
			err:         systemone.ErrUnavailable,
			wantOutcome: statistics.OutcomeBackendFailed,
			wantClass:   statistics.InferenceUnavailable,
		},
		{
			name:        "malformed response classifies invalid response",
			profile:     adminconfig.ProfileTalkLow,
			err:         fmt.Errorf("%w: cannot decode", systemone.ErrMalformed),
			wantOutcome: statistics.OutcomeInvalidResponse,
			wantClass:   statistics.InferenceInvalidResponse,
		},
		{
			name:        "pretransport rejection is an outcome-level invalid response",
			profile:     adminconfig.ProfileTalkLow,
			err:         fmt.Errorf("%w: model is required", systemone.ErrInvalidRequest),
			wantOutcome: statistics.OutcomeInvalidResponse,
			wantClass:   statistics.InferencePretransportRejected,
		},
		{
			name:        "client not-configured classifies backend failed",
			profile:     adminconfig.ProfileTalkLow,
			err:         systemone.ErrNotConfigured,
			wantOutcome: statistics.OutcomeBackendFailed,
			wantClass:   statistics.InferenceNotConfigured,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := newProactiveTestBot(t, func(s *adminStoreStub) {
				s.DefaultInteractivityMode = adminconfig.InteractivityMentionsReplies
				s.DefaultInteractionProfile = tc.profile
				s.SystemOneModel = "decision-model"
				s.ChatModel = "generation-model"
			})

			client := &fakeDecisionClient{answers: tc.answers, err: tc.err}
			stop := p.bot.SetProactiveDecisions(client, &fakePersonaSource{}, 96*1024, "route-model")
			t.Cleanup(stop)

			p.deliver(t, proactiveGroupUpdate("routine chatter"))

			snap := waitForInteraction(t, p.bot.stats, func(s statistics.InteractionSnapshot) bool {
				return s.Decision.Totals.Outcomes.Sum() > 0
			})
			assertLedgerInvariants(t, snap)

			if !outcomeCounted(snap.Decision.Totals.Outcomes, tc.wantOutcome) {
				t.Fatalf("outcome = %+v, want exactly one %d", snap.Decision.Totals.Outcomes, tc.wantOutcome)
			}
			if snap.Decision.Totals.EvaluatedAttempts != 1 {
				t.Fatalf("evaluated_attempts = %d, want 1", snap.Decision.Totals.EvaluatedAttempts)
			}

			// The success class is not an error counter: the attempt's
			// success is asserted via evaluated_attempts above.
			if tc.wantClass != statistics.InferenceSuccess {
				if got := inferenceClassCount(snap.Decision.Totals.InferenceErrors, tc.wantClass); got != 1 {
					t.Fatalf("inference class %d = %d, want 1 (classes %+v)", tc.wantClass, got, snap.Decision.Totals.InferenceErrors)
				}
			}

			scoreTotal := histogramTotal(snap.Decision.Totals.ScoreHistograms.Base)
			if tc.wantScores && scoreTotal != 1 {
				t.Fatalf("evaluated decision must record scores, got %d", scoreTotal)
			}
			if !tc.wantScores && scoreTotal != 0 {
				t.Fatalf("unevaluated decision must record no scores, got %d", scoreTotal)
			}
		})
	}
}

// TestDecisionOutcomeMappingUnit pins the err-first classification of the
// mapping helper for the engine's typed errors, including the defensive
// unreachable-in-production classes.
func TestDecisionOutcomeMappingUnit(t *testing.T) {
	evidenceInvalid := interactivity.ActionDecision{Action: interactivity.ActionIgnore, Reason: interactivity.ReasonEvidenceInvalid}

	cases := []struct {
		err    error
		reason interactivity.DecisionReason
		want   statistics.InteractionOutcome
	}{
		{nil, interactivity.ReasonProfileDisabled, statistics.OutcomeNoopProfileDisabled},
		{nil, interactivity.ReasonUnconfigured, statistics.OutcomeUnconfigured},
		{nil, interactivity.ReasonEvidenceInvalid, statistics.OutcomeEvidenceInvalid},
		{nil, interactivity.ReasonIntrusionVeto, statistics.OutcomeSelectedIgnoreVeto},
		{nil, interactivity.ReasonBelowThresholds, statistics.OutcomeSelectedIgnoreBelow},
		{nil, interactivity.ReasonReact, statistics.OutcomeSelectedReact},
		{nil, interactivity.ReasonReply, statistics.OutcomeSelectedReply},
		{interactivity.ErrUnconfigured, interactivity.ReasonUnconfigured, statistics.OutcomeUnconfigured},
		{fmt.Errorf("%w: db down", interactivity.ErrModelResolution), evidenceInvalid.Reason, statistics.OutcomePreparationFailed},
		{fmt.Errorf("%w: no room", interactivity.ErrContextTooLarge), evidenceInvalid.Reason, statistics.OutcomePreparationFailed},
		{interactivity.ErrHistoryUnavailable, evidenceInvalid.Reason, statistics.OutcomePreparationFailed},
		{interactivity.ErrTargetUnavailable, evidenceInvalid.Reason, statistics.OutcomePreparationFailed},
		{interactivity.ErrInvalidResponse, evidenceInvalid.Reason, statistics.OutcomeInvalidResponse},
		{interactivity.ErrBackendUnavailable, evidenceInvalid.Reason, statistics.OutcomeBackendFailed},
		{fmt.Errorf("%w: %w", interactivity.ErrBackendUnavailable, context.DeadlineExceeded), evidenceInvalid.Reason, statistics.OutcomeBackendTimeout},
	}

	for _, tc := range cases {
		decision := interactivity.ActionDecision{Reason: tc.reason}
		if got := decisionOutcome(tc.err, decision); got != tc.want {
			t.Fatalf("decisionOutcome(%v, reason %v) = %v, want %v", tc.err, tc.reason, got, tc.want)
		}
	}

	// The err-first rule: a carried evidence-invalid reason must not change
	// the classification of a non-nil error.
	for _, tc := range cases {
		if tc.err == nil {
			continue
		}
		if got := decisionOutcome(tc.err, evidenceInvalid); got != tc.want {
			t.Fatalf("error-first mapping broken for %v: got %v, want %v", tc.err, got, tc.want)
		}
	}

	// Score extraction: only evaluated decisions carry scores, and the base
	// is recomputed via the policy formula.
	scores := interactivity.DecisionScores{Relevance: 1, Intrusion: 0.1, Contribution: 1, ReactSuitable: 1, ReplySuitable: 0}
	profile := profileOf(adminconfig.ProfileTalkLow)
	if decisionScoresOf(profile, interactivity.ActionDecision{Reason: interactivity.ReasonProfileDisabled}) != nil {
		t.Fatalf("profile-disabled decisions carry no scores")
	}
	if decisionScoresOf(profile, interactivity.ActionDecision{Reason: interactivity.ReasonUnconfigured}) != nil {
		t.Fatalf("unconfigured decisions carry no scores")
	}
	got := decisionScoresOf(profile, interactivity.ActionDecision{Reason: interactivity.ReasonReact, Scores: scores})
	if got == nil {
		t.Fatalf("react decision must carry scores")
	}
	if got.Base != interactivity.Base(profile, scores) {
		t.Fatalf("base score must come from interactivity.Base")
	}
}

// TestInteractionSubmissionLedgerShutdown pins the ledger classes at the
// runner boundary: a closed runner rejects with submit_rejected (the task
// never exists), and an accepted task cancelled while parked on its chat
// slot surfaces exactly one accepted_then_cancelled — never a decision
// outcome, never a rejection. The re-check exit shares this same discard
// hook; it is racy to force deterministically and stays covered by the
// hook's single call site.
func TestInteractionSubmissionLedgerShutdown(t *testing.T) {
	p := newProactiveTestBot(t, func(s *adminStoreStub) {
		s.DefaultInteractivityMode = adminconfig.InteractivityMentionsReplies
		s.DefaultInteractionProfile = adminconfig.ProfileTalkLow
		s.SystemOneModel = "decision-model"
	})

	// The first task blocks in the engine until the runner shuts down, so
	// the second task of the same chat parks on the slot.
	engine := p.wireFakeEngine(t)
	engine.script = func(ctx context.Context, _ interactivity.DecisionInput) (interactivity.ActionDecision, error) {
		<-ctx.Done()

		return interactivity.ActionDecision{}, ctx.Err()
	}

	p.deliver(t, proactiveGroupUpdate("holder message"))
	waitFor(t, func() bool { return engine.calls() == 1 })

	// Parked while the slot is occupied.
	p.deliver(t, proactiveGroupUpdate("parked message"))
	time.Sleep(150 * time.Millisecond) // settle: the second task reached the park

	// Shut down: the holder's decision classifies through the normal engine
	// path (context cancellation), the parked task is discarded.
	p.bot.participation.Close()

	snap := waitForInteraction(t, p.bot.stats, func(s statistics.InteractionSnapshot) bool {
		return s.Submission.AcceptedThenCancelled == 1
	})
	assertLedgerInvariants(t, snap)

	if snap.Submission.Submitted != 2 {
		t.Fatalf("submitted = %d, want 2", snap.Submission.Submitted)
	}
	if snap.Submission.SubmitRejected != 0 {
		t.Fatalf("submit_rejected = %d, want 0 (both tasks were accepted)", snap.Submission.SubmitRejected)
	}
	if snap.Decision.Totals.Outcomes.BackendCancelled != 1 {
		t.Fatalf("the holder task must classify backend_cancelled, got %+v", snap.Decision.Totals.Outcomes)
	}

	// A message arriving after shutdown: the collection point records the
	// submission, the closed runner rejects it, and the rejection lands in
	// the ledger (Submit after Close returns false without a task).
	p.deliver(t, proactiveGroupUpdate("after shutdown"))

	snap = waitForInteraction(t, p.bot.stats, func(s statistics.InteractionSnapshot) bool {
		return s.Submission.SubmitRejected == 1
	})
	assertLedgerInvariants(t, snap)

	if snap.Submission.Submitted != 3 {
		t.Fatalf("submitted = %d, want 3", snap.Submission.Submitted)
	}
	if accepted := snap.Submission.Accepted(); accepted != 2 {
		t.Fatalf("accepted = %d, want 2 (submitted minus the rejected one)", accepted)
	}
	if snap.Decision.Totals.Outcomes.BackendCancelled != 1 {
		t.Fatalf("the holder task must classify backend_cancelled, got %+v", snap.Decision.Totals.Outcomes)
	}

	chatLedger := p.bot.stats.InteractionSnapshot(statistics.ChatScope(-100999), statistics.InteractionFilters{})
	if chatLedger.Submission == nil || chatLedger.Submission.SubmitRejected != 1 {
		t.Fatalf("closed-runner rejection must land in the ledger, got %+v", chatLedger.Submission)
	}
}

// TestInteractionPreflightKinds pins the preflight classifications that are
// reachable in the harness. config_resolution_failed (needs a settings read
// failing between the handler gate and the task snapshot) and
// engine_wiring_failed (needs NewEngine to reject valid production wiring)
// are defensive-only here.
func TestInteractionPreflightKinds(t *testing.T) {
	t.Run("access revoked while queued", func(t *testing.T) {
		p := newProactiveTestBot(t, func(s *adminStoreStub) {
			s.DefaultInteractivityMode = adminconfig.InteractivityMentionsReplies
			s.DefaultInteractionProfile = adminconfig.ProfileTalkLow
			s.SystemOneModel = "decision-model"
		})

		release := make(chan struct{})
		engine := p.wireFakeEngine(t)
		engine.script = func(_ context.Context, _ interactivity.DecisionInput) (interactivity.ActionDecision, error) {
			<-release

			return interactivity.ActionDecision{Action: interactivity.ActionIgnore, Reason: interactivity.ReasonBelowThresholds}, nil
		}

		// The holder occupies the chat slot; the second task parks behind it.
		p.deliver(t, proactiveGroupUpdate("holder"))
		waitFor(t, func() bool { return engine.calls() == 1 })
		p.deliver(t, proactiveGroupUpdate("revoked while parked"))
		time.Sleep(150 * time.Millisecond)

		// Revoke access, then let the parked task run its re-check.
		p.store.policy = adminconfig.AccessPolicyAllowlist
		close(release)

		snap := waitForInteraction(t, p.bot.stats, func(s statistics.InteractionSnapshot) bool {
			return s.Preflight.Totals.AccessDenied == 1
		})
		assertLedgerInvariants(t, snap)
		if snap.Decision.Totals.Outcomes.SelectedIgnoreBelow != 1 {
			t.Fatalf("the holder must decide normally, got %+v", snap.Decision.Totals.Outcomes)
		}
	})

	t.Run("mode flipped to disabled while queued", func(t *testing.T) {
		p := newProactiveTestBot(t, func(s *adminStoreStub) {
			s.DefaultInteractivityMode = adminconfig.InteractivityMentionsReplies
			s.DefaultInteractionProfile = adminconfig.ProfileTalkLow
			s.SystemOneModel = "decision-model"
		})

		release := make(chan struct{})
		engine := p.wireFakeEngine(t)
		engine.script = func(_ context.Context, _ interactivity.DecisionInput) (interactivity.ActionDecision, error) {
			<-release

			return interactivity.ActionDecision{Action: interactivity.ActionIgnore, Reason: interactivity.ReasonBelowThresholds}, nil
		}

		p.deliver(t, proactiveGroupUpdate("holder"))
		waitFor(t, func() bool { return engine.calls() == 1 })
		p.deliver(t, proactiveGroupUpdate("flipped while parked"))
		time.Sleep(150 * time.Millisecond)

		p.store.DefaultInteractivityMode = adminconfig.InteractivityDisabled
		close(release)

		snap := waitForInteraction(t, p.bot.stats, func(s statistics.InteractionSnapshot) bool {
			return s.Preflight.Totals.EligibilityLost == 1
		})
		assertLedgerInvariants(t, snap)
	})

	t.Run("persona capture failed", func(t *testing.T) {
		p := newProactiveTestBot(t, func(s *adminStoreStub) {
			s.DefaultInteractivityMode = adminconfig.InteractivityMentionsReplies
			s.DefaultInteractionProfile = adminconfig.ProfileTalkLow
			s.SystemOneModel = "decision-model"
		})

		persona := &fakePersonaSource{captureErr: errors.New("template store down")}
		engine := p.wireFakeEngine(t)
		p.bot.decisions.persona = persona

		p.deliver(t, proactiveGroupUpdate("persona-less chatter"))

		snap := waitForInteraction(t, p.bot.stats, func(s statistics.InteractionSnapshot) bool {
			return s.Preflight.Totals.PersonaCaptureFailed == 1
		})
		assertLedgerInvariants(t, snap)
		if engine.calls() != 0 {
			t.Fatalf("engine must not run when persona capture fails")
		}
	})
}

// TestInteractionExecutionEffectClasses pins the execution ledger over the
// real turn pipeline: dispatch before, completion after, and the four
// effect/failure classes with their overlap semantics.
func TestInteractionExecutionEffectClasses(t *testing.T) {
	newReplyBot := func(t *testing.T) *proactiveTestBot {
		p := newProactiveTestBot(t, func(s *adminStoreStub) {
			s.DefaultInteractivityMode = adminconfig.InteractivityMentionsReplies
			s.DefaultInteractionProfile = adminconfig.ProfileTalkLow
			s.SystemOneModel = "decision-model"
			s.ChatModel = "generation-model"
		})

		// Every case decides Reply.
		client := &fakeDecisionClient{answers: decisionAnswers(0.05, 0.0, 1.0)}
		stop := p.bot.SetProactiveDecisions(client, &fakePersonaSource{}, 96*1024, "route-model")
		t.Cleanup(stop)

		return p
	}

	// clearErr restores the stub after an error-scripted case.
	clearErr := func(p *proactiveTestBot) {
		t.Cleanup(func() {
			p.llm.mu.Lock()
			p.llm.err = nil
			p.llm.mu.Unlock()
		})
	}

	for _, precedingTool := range []string{"", "set_reaction", "send_dice"} {
		t.Run("intentional silence after "+precedingTool, func(t *testing.T) {
			p := newReplyBot(t)
			var calls []llm.ToolCall
			if precedingTool != "" {
				args := []byte(`{}`)
				if precedingTool == "set_reaction" {
					args = []byte(`{"emoji":"🎉"}`)
				}
				calls = append(calls, llm.ToolCall{ID: "effect", Name: precedingTool, Arguments: args})
			}
			calls = append(calls, llm.ToolCall{ID: "silence", Name: "say_nothing", Arguments: []byte(`{}`)})
			p.llm.mu.Lock()
			p.llm.responses = []llm.Response{{Message: llm.Message{Role: llm.RoleAssistant, ToolCalls: calls}}}
			p.llm.mu.Unlock()
			p.deliver(t, proactiveGroupUpdate("reply-worthy message"))
			snapshot := waitForInteraction(t, p.bot.stats, func(s statistics.InteractionSnapshot) bool {
				return s.Execution.Totals.TurnsWithEffect+s.Execution.Totals.TurnsWithoutEffect+s.Execution.Totals.TurnsFailed == 1
			})
			exec := snapshot.Execution.Totals
			if exec.DispatchedReply != 1 || exec.TurnsFailed != 0 || exec.TextsConfirmed != 0 || p.llm.buildCount() != 1 {
				t.Fatalf("silence must complete successfully without text: %+v", exec)
			}
			if (exec.ReactionsConfirmed == 1) != (precedingTool == "set_reaction") || (exec.OtherSideEffects == 1) != (precedingTool == "send_dice") {
				t.Fatalf("confirmed effects changed: %+v", exec)
			}
			if (exec.TurnsWithoutEffect == 1) != (precedingTool == "") || (exec.TurnsWithEffect == 1) != (precedingTool != "") {
				t.Fatalf("terminal tool must not count as a side effect: %+v", exec)
			}
		})
	}

	t.Run("reaction-only turn via restricted react decision", func(t *testing.T) {
		p := newProactiveTestBot(t, func(s *adminStoreStub) {
			s.DefaultInteractivityMode = adminconfig.InteractivityMentionsReplies
			s.DefaultInteractionProfile = adminconfig.ProfileReactionsHigh
			s.SystemOneModel = "decision-model"
			s.ChatModel = "generation-model"
		})

		client := &fakeDecisionClient{answers: decisionAnswers(0.1, 1.0, 0.0)}
		stop := p.bot.SetProactiveDecisions(client, &fakePersonaSource{}, 96*1024, "route-model")
		t.Cleanup(stop)
		p.llm.mu.Lock()
		p.llm.responses = []llm.Response{
			{Message: llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{
				{ID: "call-1", Name: "set_reaction", Arguments: []byte(`{"emoji":"🎉"}`)},
			}}},
		}
		p.llm.mu.Unlock()

		p.deliver(t, proactiveGroupUpdate("react-worthy"))

		waitFor(t, func() bool { return p.api.callCount("setMessageReaction") >= 1 })
		snap := waitForInteraction(t, p.bot.stats, func(s statistics.InteractionSnapshot) bool {
			return s.Execution.Totals.ReactionsConfirmed == 1
		})

		exec := snap.Execution.Totals
		if exec.DispatchedReact != 1 || exec.DispatchedReply != 0 {
			t.Fatalf("dispatched = react %d / reply %d, want 1/0", exec.DispatchedReact, exec.DispatchedReply)
		}
		if exec.ReactionsConfirmed != 1 || exec.TextsConfirmed != 0 || exec.OtherSideEffects != 0 {
			t.Fatalf("reaction-only effects wrong: %+v", exec.ExecutionCounters)
		}
		if exec.TurnsWithEffect != 1 || exec.TurnsFailed != 0 {
			t.Fatalf("turn accounting wrong: with %d, failed %d", exec.TurnsWithEffect, exec.TurnsFailed)
		}
		if exec.TurnsWithEffect+exec.TurnsWithoutEffect+exec.TurnsFailed != exec.DispatchedReact+exec.DispatchedReply {
			t.Fatalf("every dispatched turn must complete: %+v", exec.ExecutionCounters)
		}
	})

	t.Run("text-only reply turn", func(t *testing.T) {
		p := newReplyBot(t)

		p.deliver(t, proactiveGroupUpdate("text question"))
		waitFor(t, func() bool { return p.deliveredCount() >= 1 })

		snap := waitForInteraction(t, p.bot.stats, func(s statistics.InteractionSnapshot) bool {
			return s.Execution.Totals.TextsConfirmed == 1
		})
		exec := snap.Execution.Totals
		if exec.ReactionsConfirmed != 0 || exec.OtherSideEffects != 0 {
			t.Fatalf("text-only turn must record only the text: %+v", exec.ExecutionCounters)
		}
		if exec.TurnsWithEffect != 1 || exec.TurnsFailed != 0 {
			t.Fatalf("turn accounting wrong: %+v", exec.ExecutionCounters)
		}
	})

	t.Run("reply with reaction and other side effect sets all three flags", func(t *testing.T) {
		p := newReplyBot(t)
		p.llm.mu.Lock()
		p.llm.responses = []llm.Response{
			{Message: llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{
				{ID: "call-1", Name: "set_reaction", Arguments: []byte(`{"emoji":"🔥"}`)},
			}}},
			{Message: llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{
				{ID: "call-2", Name: "send_dice", Arguments: []byte(`{"emoji":"🎲"}`)},
			}}},
			{Message: llm.TextMessage(llm.RoleAssistant, "and here is my answer")},
		}
		p.llm.mu.Unlock()

		p.deliver(t, proactiveGroupUpdate("do all the things"))
		waitFor(t, func() bool { return p.deliveredCount() >= 1 })

		snap := waitForInteraction(t, p.bot.stats, func(s statistics.InteractionSnapshot) bool {
			return s.Execution.Totals.OtherSideEffects == 1
		})
		exec := snap.Execution.Totals
		// Per-call classifications are exclusive; the TURN-level flags
		// co-occur: one turn, reaction AND other effect AND text.
		if exec.ReactionsConfirmed != 1 || exec.TextsConfirmed != 1 || exec.OtherSideEffects != 1 {
			t.Fatalf("co-occurring flags wrong: %+v", exec.ExecutionCounters)
		}
		if exec.TurnsWithEffect != 1 {
			t.Fatalf("one participating turn must count once: %+v", exec.ExecutionCounters)
		}
		if p.api.callCount("sendDice") != 1 {
			t.Fatalf("the dice tool must actually run")
		}
	})

	t.Run("successful reaction turn without effects", func(t *testing.T) {
		// A reaction turn whose only tool call is refused (album target)
		// completes successfully with its text suppressed: no confirmed
		// effect anywhere, and no failure — turns_without_effect.
		p := newProactiveTestBot(t, func(s *adminStoreStub) {
			s.DefaultInteractivityMode = adminconfig.InteractivityMentionsReplies
			s.DefaultInteractionProfile = adminconfig.ProfileReactionsHigh
			s.SystemOneModel = "decision-model"
			s.ChatModel = "generation-model"
		})

		client := &fakeDecisionClient{answers: decisionAnswers(0.1, 1.0, 0.0)}
		stop := p.bot.SetProactiveDecisions(client, &fakePersonaSource{}, 96*1024, "route-model")
		t.Cleanup(stop)
		p.llm.mu.Lock()
		p.llm.responses = []llm.Response{
			{Message: llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{
				{ID: "call-1", Name: "set_reaction", Arguments: []byte(`{"emoji":"🎉"}`)},
			}}},
			{Message: llm.TextMessage(llm.RoleAssistant, "suppressed fallback text")},
		}
		p.llm.mu.Unlock()

		refused := proactiveGroupUpdate("album target")
		refused.Message.MediaGroupID = "album-1"
		p.deliver(t, refused)
		time.Sleep(200 * time.Millisecond)

		snap := waitForInteraction(t, p.bot.stats, func(s statistics.InteractionSnapshot) bool {
			return s.Execution.Totals.TurnsWithoutEffect == 1
		})
		exec := snap.Execution.Totals
		if exec.TurnsWithEffect != 0 || exec.TurnsFailed != 0 || exec.ReactionsConfirmed != 0 {
			t.Fatalf("effect-free success must land only in turns_without_effect: %+v", exec.ExecutionCounters)
		}
		if p.api.callCount("setMessageReaction") != 0 {
			t.Fatalf("the album-refused reaction must never reach the API")
		}
	})

	t.Run("partial success counts the reaction and the failure", func(t *testing.T) {
		p := newReplyBot(t)
		// The reaction applies on the first generation; the SECOND request
		// construction fails (buildErr), so the run fails while keeping its
		// tool-call records — the partial-success shape.
		p.llm.mu.Lock()
		p.llm.responses = []llm.Response{
			{Message: llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{
				{ID: "call-1", Name: "set_reaction", Arguments: []byte(`{"emoji":"🎉"}`)},
			}}},
		}
		p.llm.buildErr = fmt.Errorf("generation exploded after the reaction")
		p.llm.buildErrAfter = 1 // build #1 succeeds; build #2 fails
		p.llm.mu.Unlock()
		t.Cleanup(func() {
			p.llm.mu.Lock()
			p.llm.buildErr = nil
			p.llm.mu.Unlock()
		})

		p.deliver(t, proactiveGroupUpdate("reaction then doom"))
		waitFor(t, func() bool { return p.api.callCount("setMessageReaction") >= 1 })

		snap := waitForInteraction(t, p.bot.stats, func(s statistics.InteractionSnapshot) bool {
			return s.Execution.Totals.TurnsFailed == 1
		})
		exec := snap.Execution.Totals
		// The applied reaction is confirmed regardless of completion, and
		// the failed generation is a failure: both facts are true.
		if exec.ReactionsConfirmed != 1 {
			t.Fatalf("the applied reaction must be confirmed: %+v", exec.ExecutionCounters)
		}
		if exec.TurnsFailed != 1 || exec.Failures.Generation != 1 {
			t.Fatalf("the failed generation must classify: %+v", exec.ExecutionCounters)
		}
		if exec.TurnsWithEffect != 1 {
			t.Fatalf("a partial success is a turn with effect: %+v", exec.ExecutionCounters)
		}
	})

	t.Run("delivery failure classifies delivery", func(t *testing.T) {
		p := newReplyBot(t)
		// Take down every send path of the delivery helper: the rich route
		// (retried across fallbacks) and the legacy route.
		p.api.failNext("sendRichMessage", 3)
		p.api.failNext("sendMessage", 3)

		p.deliver(t, proactiveGroupUpdate("undeliverable answer"))
		time.Sleep(200 * time.Millisecond)

		snap := waitForInteraction(t, p.bot.stats, func(s statistics.InteractionSnapshot) bool {
			return s.Execution.Totals.Failures.Delivery == 1
		})
		exec := snap.Execution.Totals
		if exec.TextsConfirmed != 0 {
			t.Fatalf("a failed delivery must not confirm text: %+v", exec.ExecutionCounters)
		}
		if exec.TurnsFailed != 1 || exec.TurnsWithEffect != 0 {
			t.Fatalf("turn accounting wrong: %+v", exec.ExecutionCounters)
		}
	})

	t.Run("pipeline timeout sentinel classifies timeout", func(t *testing.T) {
		p := newReplyBot(t)
		p.llm.mu.Lock()
		p.llm.err = fmt.Errorf("%w: nested deadline", ErrRequestTimeout)
		p.llm.mu.Unlock()
		clearErr(p)

		p.deliver(t, proactiveGroupUpdate("slow conversation"))
		snap := waitForInteraction(t, p.bot.stats, func(s statistics.InteractionSnapshot) bool {
			return s.Execution.Totals.Failures.Timeout == 1
		})
		if snap.Execution.Totals.Failures.Generation != 0 {
			t.Fatalf("timeout must not classify as generation: %+v", snap.Execution.Totals.ExecutionCounters)
		}
	})

	t.Run("cancellation keeps its own class", func(t *testing.T) {
		p := newReplyBot(t)
		p.llm.mu.Lock()
		p.llm.err = context.Canceled
		p.llm.mu.Unlock()
		clearErr(p)

		p.deliver(t, proactiveGroupUpdate("cancelled conversation"))
		waitForInteraction(t, p.bot.stats, func(s statistics.InteractionSnapshot) bool {
			return s.Execution.Totals.Failures.Cancelled == 1
		})
	})

	t.Run("unclassified error is generation", func(t *testing.T) {
		p := newReplyBot(t)
		p.llm.mu.Lock()
		p.llm.err = errors.New("mysterious failure")
		p.llm.mu.Unlock()
		clearErr(p)

		p.deliver(t, proactiveGroupUpdate("doomed conversation"))
		waitForInteraction(t, p.bot.stats, func(s statistics.InteractionSnapshot) bool {
			return s.Execution.Totals.Failures.Generation == 1
		})
	})
}

// TestInteractionDispatchPrecedesCompletion pins the tier semantics: while
// the turn is in flight, dispatch is already counted and completion is not;
// after the drain both equal.
func TestInteractionDispatchPrecedesCompletion(t *testing.T) {
	p := newProactiveTestBot(t, func(s *adminStoreStub) {
		s.DefaultInteractivityMode = adminconfig.InteractivityMentionsReplies
		s.DefaultInteractionProfile = adminconfig.ProfileTalkLow
		s.SystemOneModel = "decision-model"
	})

	client := &fakeDecisionClient{answers: decisionAnswers(0.05, 0.0, 1.0)}
	stop := p.bot.SetProactiveDecisions(client, &fakePersonaSource{}, 96*1024, "route-model")
	t.Cleanup(stop)

	// Hold the generation mid-turn so dispatch and completion separate.
	gate := make(chan struct{})
	var openGate sync.Once
	p.llm.mu.Lock()
	p.llm.gate = gate
	p.llm.mu.Unlock()
	t.Cleanup(func() {
		openGate.Do(func() { close(gate) }) // safety net for Fatalf exits
	})

	p.deliver(t, proactiveGroupUpdate("in-flight question"))

	// Dispatch recorded while the turn is blocked in generation.
	inFlight := waitForInteraction(t, p.bot.stats, func(s statistics.InteractionSnapshot) bool {
		return s.Execution.Totals.DispatchedReply == 1
	})
	completed := inFlight.Execution.Totals.TextsConfirmed +
		inFlight.Execution.Totals.TurnsWithEffect +
		inFlight.Execution.Totals.TurnsFailed +
		inFlight.Execution.Totals.TurnsWithoutEffect
	if completed != 0 {
		t.Fatalf("no completion may be recorded while the turn is in flight, got %d", completed)
	}

	// Release the turn; delivery and the completion observation follow.
	openGate.Do(func() { close(gate) })

	waitFor(t, func() bool { return p.deliveredCount() >= 1 })

	snap := waitForInteraction(t, p.bot.stats, func(s statistics.InteractionSnapshot) bool {
		return s.Execution.Totals.TextsConfirmed == 1
	})
	assertLedgerInvariants(t, snap)
	if snap.Execution.Totals.DispatchedReply != 1 || snap.Execution.Totals.TextsConfirmed != 1 {
		t.Fatalf("after the drain dispatch equals completion: %+v", snap.Execution.Totals.ExecutionCounters)
	}
}

// TestInteractionExplicitTurnsLeaveInteractionUntouched pins the boundary:
// explicit conversation flows through the same pipeline but never touches
// the interaction dataset.
func TestInteractionExplicitTurnsLeaveInteractionUntouched(t *testing.T) {
	p := newProactiveTestBot(t, func(s *adminStoreStub) {
		s.DefaultInteractivityMode = adminconfig.InteractivityMentionsReplies
		s.DefaultInteractionProfile = adminconfig.ProfileTalkLow
	})

	// No proactive wiring at all: b.decisions is nil, so an explicit turn
	// cannot leak into interaction counters even indirectly.
	p.deliver(t, proactiveMentionUpdate())
	waitFor(t, func() bool { return p.deliveredCount() >= 1 })

	snap := p.bot.stats.InteractionSnapshot(statistics.GlobalScope(), statistics.InteractionFilters{})
	if snap.Submission.Submitted != 0 || snap.Decision.Totals.Outcomes.Sum() != 0 || snap.Execution.Totals.DispatchedReply != 0 {
		t.Fatalf("explicit turns must leave the interaction dataset untouched: %+v", snap)
	}
}

// TestInteractionEffectiveGenerationModelFallback pins that an empty chat
// model override records the environment route model as the effective
// generation model, never an empty identifier.
func TestInteractionEffectiveGenerationModelFallback(t *testing.T) {
	p := newProactiveTestBot(t, func(s *adminStoreStub) {
		s.DefaultInteractivityMode = adminconfig.InteractivityMentionsReplies
		s.DefaultInteractionProfile = adminconfig.ProfileTalkLow
		s.SystemOneModel = "decision-model"
		// ChatModel unset: the generation falls back to the ENV route.
	})

	client := &fakeDecisionClient{answers: decisionAnswers(0.05, 0.0, 1.0)}
	stop := p.bot.SetProactiveDecisions(client, &fakePersonaSource{}, 96*1024, "route-model")
	t.Cleanup(stop)

	p.deliver(t, proactiveGroupUpdate("who made you"))
	waitFor(t, func() bool { return p.deliveredCount() >= 1 })

	snap := waitForInteraction(t, p.bot.stats, func(s statistics.InteractionSnapshot) bool {
		return s.Execution.Totals.TextsConfirmed == 1
	})
	if len(snap.Execution.Buckets) != 1 {
		t.Fatalf("one execution bucket expected, got %d", len(snap.Execution.Buckets))
	}
	b := snap.Execution.Buckets[0]
	if b.GenerationModel == nil || *b.GenerationModel != "route-model" {
		t.Fatalf("generation model must be the ENV route model, got %v", b.GenerationModel)
	}
	if b.DecisionModel == nil || *b.DecisionModel != "decision-model" {
		t.Fatalf("decision model must be retained in the execution bucket")
	}
}

// TestInteractionResetDuringActiveInference pins reset-epoch consistency in
// the harness: a reset while a decision is actively inferring drops the
// whole old-epoch operation — its submission and every later observation —
// while the actual operation is neither cancelled nor delayed.
func TestInteractionResetDuringActiveInference(t *testing.T) {
	p := newProactiveTestBot(t, func(s *adminStoreStub) {
		s.DefaultInteractivityMode = adminconfig.InteractivityMentionsReplies
		s.DefaultInteractionProfile = adminconfig.ProfileTalkLow
		s.SystemOneModel = "decision-model"
	})

	// Gate the client mid-Evaluate: the operation is in active inference.
	gate := make(chan struct{})
	client := &fakeDecisionClient{answers: decisionAnswers(0.05, 0.0, 1.0), gate: gate}
	stop := p.bot.SetProactiveDecisions(client, &fakePersonaSource{}, 96*1024, "route-model")
	t.Cleanup(stop)

	p.deliver(t, proactiveGroupUpdate("inferring"))
	waitFor(t, func() bool { return client.calls() == 1 })

	// Reset statistics while inference is in flight.
	p.bot.stats.ResetInteraction()

	close(gate) // the operation completes normally — never cancelled
	waitFor(t, func() bool { return p.deliveredCount() >= 1 })
	time.Sleep(150 * time.Millisecond) // settle: execution observation attempted

	snap := p.bot.stats.InteractionSnapshot(statistics.GlobalScope(), statistics.InteractionFilters{})
	// The new period contains NOTHING of the old operation: its submission,
	// decision, inference, and execution records all dropped together even
	// though the operation completed after the reset.
	if snap.Submission.Submitted != 0 {
		t.Fatalf("old-epoch submission must be gone: %d", snap.Submission.Submitted)
	}
	if snap.Decision.Totals.Outcomes.Sum() != 0 {
		t.Fatalf("old-epoch outcome must not enter the new period: %+v", snap.Decision.Totals.Outcomes)
	}
	if snap.Execution.Totals.DispatchedReply != 0 || snap.Execution.Totals.TextsConfirmed != 0 {
		t.Fatalf("old-epoch execution must not enter the new period")
	}
	if snap.Decision.Totals.EvaluatedAttempts != 0 {
		t.Fatalf("old-epoch inference must not enter the new period")
	}
	if snap.Period.Resets != 1 {
		t.Fatalf("period resets = %d, want 1", snap.Period.Resets)
	}
}

// TestInteractionModeFlipQueuedTaskZeroContamination pins that a queued task
// surviving a mode flip classifies ONLY through the preflight (never through
// the engine), keeping the decision buckets uncontaminated.
func TestInteractionModeFlipQueuedTaskZeroContamination(t *testing.T) {
	p := newProactiveTestBot(t, func(s *adminStoreStub) {
		s.DefaultInteractivityMode = adminconfig.InteractivityMentionsReplies
		s.DefaultInteractionProfile = adminconfig.ProfileTalkLow
		s.SystemOneModel = "decision-model"
	})

	decided := atomic.Int32{}
	release := make(chan struct{})
	engine := p.wireFakeEngine(t)
	engine.script = func(_ context.Context, _ interactivity.DecisionInput) (interactivity.ActionDecision, error) {
		decided.Add(1)
		<-release

		return interactivity.ActionDecision{Action: interactivity.ActionIgnore, Reason: interactivity.ReasonBelowThresholds}, nil
	}

	p.deliver(t, proactiveGroupUpdate("holder"))
	waitFor(t, func() bool { return engine.calls() == 1 })
	p.deliver(t, proactiveGroupUpdate("queued behind"))
	time.Sleep(150 * time.Millisecond)

	p.store.DefaultInteractivityMode = adminconfig.InteractivityDisabled
	close(release)

	snap := waitForInteraction(t, p.bot.stats, func(s statistics.InteractionSnapshot) bool {
		return s.Preflight.Totals.EligibilityLost == 1
	})
	if decided.Load() != 1 {
		t.Fatalf("only the holder may reach the engine, got %d decisions", decided.Load())
	}
	// The holder is the only decision record; the queued task terminated in
	// the preflight.
	if snap.Decision.Totals.Outcomes.SelectedIgnoreBelow != 1 {
		t.Fatalf("the holder outcome must be the only decision record: %+v", snap.Decision.Totals.Outcomes)
	}
	assertLedgerInvariants(t, snap)
}
