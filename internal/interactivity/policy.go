package interactivity

// EvaluatePolicy applies the deterministic spec §5 algorithm to one score
// set. It is pure: no randomness, no inference, no I/O. All comparisons are
// inclusive except the hard intrusion veto (exclusive means the veto value
// itself vetoes). Invalid evidence yields Ignore, never a best-effort guess.
func EvaluatePolicy(profile Profile, scores DecisionScores) ActionDecision {
	if !profile.Enabled {
		return ignoreDecision(ReasonProfileDisabled, DecisionScores{})
	}

	for _, value := range []float64{
		scores.Relevance, scores.Intrusion, scores.Contribution,
		scores.ReactSuitable, scores.ReplySuitable,
	} {
		if !validScore(value) {
			return ignoreDecision(ReasonEvidenceInvalid, DecisionScores{})
		}
	}

	// The veto overrides everything, including a maximum base score.
	if scores.Intrusion >= profile.MaxIntrusion {
		return ignoreDecision(ReasonIntrusionVeto, scores)
	}

	base := Base(profile, scores)

	// Reply first: a suitable reply dominates a suitable reaction. A
	// reaction may still be selected above the reply threshold when the
	// reply is independently unsuitable.
	if profile.AllowReply &&
		base >= profile.ReplyThreshold &&
		scores.ReplySuitable >= profile.ReplySuitabilityMin {
		return ActionDecision{Action: ActionReply, Scores: scores, Reason: ReasonReply}
	}
	if profile.AllowReact &&
		base >= profile.ReactionThreshold &&
		scores.ReactSuitable >= profile.ReactionSuitabilityMin {
		return ActionDecision{Action: ActionReact, Scores: scores, Reason: ReasonReact}
	}

	return ignoreDecision(ReasonBelowThresholds, scores)
}

func ignoreDecision(reason DecisionReason, scores DecisionScores) ActionDecision {
	return ActionDecision{Action: ActionIgnore, Scores: scores, Reason: reason}
}

// Base computes the policy base score of one score set under a profile: the
// weighted relevance and contribution minus the scaled intrusion penalty,
// clamped to [0,1]. It is the exact formula EvaluatePolicy applies, exported
// so external aggregation (interaction statistics) can record the same base
// score the policy gated on without reimplementing it.
func Base(profile Profile, scores DecisionScores) float64 {
	base := profile.RelevanceWeight*scores.Relevance +
		profile.ContributionWeight*scores.Contribution -
		profile.IntrusionPenalty*scores.Intrusion
	if base < 0 {
		return 0
	}
	if base > 1 {
		return 1
	}

	return base
}
