package interactivity

import (
	"math"
	"testing"

	"telegram-ollama-reply-bot/internal/adminconfig"
)

// scores builds a DecisionScores with all-invalid fields replaced.
func scores(relevance, intrusion, contribution, react, reply float64) DecisionScores {
	return DecisionScores{
		Relevance:     relevance,
		Intrusion:     intrusion,
		Contribution:  contribution,
		ReactSuitable: react,
		ReplySuitable: reply,
	}
}

// TestEvaluatePolicySpecExamples pins the worked examples from spec §5
// (Talk-Low: react .50 / reply .90 / penalty .50).
func TestEvaluatePolicySpecExamples(t *testing.T) {
	profile, ok := ProfileByID(adminconfig.ProfileTalkLow)
	if !ok {
		t.Fatalf("talk-low profile missing")
	}

	cases := []struct {
		name   string
		scores DecisionScores
		want   InteractionAction
		reason DecisionReason
	}{
		{
			// base .94, reply .80, react .20 -> Reply
			name:   "high base with suitable reply",
			scores: scores(1, 0.12, 1, 0.20, 0.80),
			want:   ActionReply,
			reason: ReasonReply,
		},
		{
			// base .94, reply .20, react .80 -> React
			name:   "high base, reply unsuitable, react suitable",
			scores: scores(1, 0.12, 1, 0.80, 0.20),
			want:   ActionReact,
			reason: ReasonReact,
		},
		{
			// base .75, reply .95, react .20 -> Ignore
			name:   "below reply threshold despite perfect suitability",
			scores: scores(1, 0.5, 1, 0.20, 0.95),
			want:   ActionIgnore,
			reason: ReasonBelowThresholds,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got := EvaluatePolicy(profile, testCase.scores)
			if got.Action != testCase.want || got.Reason != testCase.reason {
				t.Fatalf("got %s/%s, want %s/%s (scores %+v)", got.Action, got.Reason, testCase.want, testCase.reason, testCase.scores)
			}
		})
	}
}

// TestEvaluatePolicyBoundaries pins the inclusive gates and the exclusive
// veto at exact values.
func TestEvaluatePolicyBoundaries(t *testing.T) {
	profile, _ := ProfileByID(adminconfig.ProfileTalkHigh) // react .30 / reply .65 / veto .80
	maxed := scores(1, 0, 1, 1, 1)                         // base 1.0

	t.Run("suitability minimums are inclusive", func(t *testing.T) {
		atMin := maxed
		atMin.ReactSuitable = 0.60 // == reactionSuitabilityMin
		if got := EvaluatePolicy(profile, atMin); got.Action != ActionReply {
			// base 1.0 >= .65 and replySuitable 1.0: reply fires first
			t.Fatalf("expected reply at maximum base, got %s", got.Action)
		}

		noReply := maxed
		noReply.ReplySuitable = 0.6999 // just below the minimum
		noReply.ReactSuitable = 0.60   // == the minimum: inclusive
		if got := EvaluatePolicy(profile, noReply); got.Action != ActionReact {
			t.Fatalf("react must pass at the exact suitability minimum, got %s", got.Action)
		}
	})

	t.Run("intrusion veto is exclusive", func(t *testing.T) {
		atVeto := maxed
		atVeto.Intrusion = 0.80 // == maxIntrusion: vetoes despite a perfect base
		got := EvaluatePolicy(profile, atVeto)
		if got.Action != ActionIgnore || got.Reason != ReasonIntrusionVeto {
			t.Fatalf("intrusion == veto value must ignore, got %s/%s", got.Action, got.Reason)
		}

		below := maxed
		below.Intrusion = 0.7999
		below.ReplySuitable = 0 // force the react gate to be the decider
		if got := EvaluatePolicy(profile, below); got.Action != ActionReact {
			t.Fatalf("intrusion just below the veto must not ignore, got %s", got.Action)
		}
	})

	t.Run("reaction window reaches the reply threshold from below", func(t *testing.T) {
		// base = .45 + .275 - .30*.30 = .635: below the .65 reply gate but
		// above the .30 reaction gate, so the reaction still fires.
		below := scores(1, 0.30, 0.5, 1, 1)
		got := EvaluatePolicy(profile, below)
		if got.Action != ActionReact {
			t.Fatalf("base below reply threshold but above react threshold must react, got %s", got.Action)
		}
	})
}

// TestEvaluatePolicyActionFlags pins that disabled actions are disabled by
// flags, not by unreachable thresholds: reaction-only profiles never reply
// even at maximum scores.
func TestEvaluatePolicyActionFlags(t *testing.T) {
	maxed := scores(1, 0, 1, 1, 1)

	for _, id := range []adminconfig.InteractionProfile{
		adminconfig.ProfileReactionsLow,
		adminconfig.ProfileReactionsHigh,
	} {
		profile, _ := ProfileByID(id)
		got := EvaluatePolicy(profile, maxed)
		if got.Action != ActionReact {
			t.Fatalf("%s must react at maximum scores, got %s", id, got.Action)
		}
	}

	// The disabled profile ignores before any gate.
	nonInteractive, _ := ProfileByID(adminconfig.ProfileNonInteractive)
	got := EvaluatePolicy(nonInteractive, maxed)
	if got.Action != ActionIgnore || got.Reason != ReasonProfileDisabled {
		t.Fatalf("disabled profile must ignore, got %s/%s", got.Action, got.Reason)
	}
}

// TestEvaluatePolicyInvalidEvidence pins that unusable evidence yields
// Ignore, never a best-effort guess or a zero-treatment.
func TestEvaluatePolicyInvalidEvidence(t *testing.T) {
	profile, _ := ProfileByID(adminconfig.ProfileTalkHigh)

	for name, bad := range map[string]DecisionScores{
		"NaN relevance":         scores(math.NaN(), 0, 1, 1, 1),
		"inf intrusion":         scores(1, math.Inf(1), 1, 1, 1),
		"negative contribution": scores(1, 0, -0.1, 1, 1),
		"react above 1":         scores(1, 0, 1, 1.1, 1),
	} {
		got := EvaluatePolicy(profile, bad)
		if got.Action != ActionIgnore || got.Reason != ReasonEvidenceInvalid {
			t.Fatalf("%s must ignore as invalid evidence, got %s/%s", name, got.Action, got.Reason)
		}
	}
}

// TestEvaluatePolicyFractionalRelevance pins the unrounded normalization:
// the raw score is a probability-weighted mean, so relevance is fractional.
// With Talk-Low (.90 reply threshold), raw relevance 2.9 gives base .895 —
// short of the reply gate but above the reaction gate, so the outcome is
// React. An implementation that quantizes relevance to the top level (1.0)
// would compute base .91 and produce Reply; this test fails there.
func TestEvaluatePolicyFractionalRelevance(t *testing.T) {
	profile, _ := ProfileByID(adminconfig.ProfileTalkLow)

	raw := 2.9
	relevance := raw / relevanceMaxLevel

	got := EvaluatePolicy(profile, scores(relevance, 0.18, 1, 1, 1))
	if got.Action != ActionReact {
		t.Fatalf("unrounded fractional relevance must fall through to react, got %s with scores %+v", got.Action, got.Scores)
	}
	if got.Scores.Relevance != relevance {
		t.Fatalf("scores must pass through unrounded, got %v want %v", got.Scores.Relevance, relevance)
	}
}

// TestEvaluatePolicyIsDeterministic pins the no-randomness rule: identical
// inputs produce identical outcomes.
func TestEvaluatePolicyIsDeterministic(t *testing.T) {
	profile, _ := ProfileByID(adminconfig.ProfileTalkLow)
	in := scores(0.7, 0.3, 0.8, 0.5, 0.75)

	first := EvaluatePolicy(profile, in)
	for range 20 {
		if got := EvaluatePolicy(profile, in); got != first {
			t.Fatalf("policy must be deterministic: %v != %v", got, first)
		}
	}
}
