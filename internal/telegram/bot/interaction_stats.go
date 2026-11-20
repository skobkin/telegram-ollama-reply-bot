package bot

import (
	"context"
	"errors"
	"time"

	"telegram-ollama-reply-bot/internal/adminconfig"
	"telegram-ollama-reply-bot/internal/interactivity"
	"telegram-ollama-reply-bot/internal/statistics"
	"telegram-ollama-reply-bot/internal/systemone"
)

// This file adapts the proactive participation flow to the statistics
// collector. Everything here is classification plumbing: bounded enums in,
// bounded enums out, error strings never parsed, no conversation content
// recorded. The collector drops observations from stale epochs itself, so
// a statistics reset can never split an operation across periods.

// interactionKeysOf builds the bucket identity of one operation from its
// settings snapshot: chat ID, interaction profile, decision model. The
// generation model is NOT a decision key — it is recorded per execution.
func interactionKeysOf(chatID int64, resolved adminconfig.ResolvedChatConfig) statistics.InteractionKeys {
	return statistics.InteractionKeys{
		ChatID:        chatID,
		Profile:       string(resolved.InteractionProfile),
		DecisionModel: resolved.SystemOneModel,
		Mode:          statistics.ModeLive,
	}
}

// effectiveGenerationModel is the model identifier the turn's generation
// will actually use: the chat override when set, otherwise the environment
// route model the generation service falls back to when the turn model is
// empty. Never an empty override string.
func (b *Bot) effectiveGenerationModel(resolved adminconfig.ResolvedChatConfig) string {
	if resolved.ChatModel != "" {
		return resolved.ChatModel
	}

	return b.chatRouteModel
}

// instrumentedDecisionClient wraps the shared System One client for one
// proactive operation: every Client.Evaluate invocation is recorded as an
// inference attempt (the `evaluated` denominator) with its latency and a
// sentinel-derived error class. It is operation-local temporary state —
// created per engine, garbage-collected with the operation. Decision tools
// in explicit turns use the raw client and never pass through here.
type instrumentedDecisionClient struct {
	inner interactivity.DecisionClient
	stats *statistics.Collector
	epoch uint64
	keys  statistics.InteractionKeys
}

func (c *instrumentedDecisionClient) Evaluate(ctx context.Context, req systemone.Request) (map[string]systemone.Answer, error) {
	started := time.Now()
	answers, err := c.inner.Evaluate(ctx, req)
	c.stats.RecordInference(statistics.InferenceAttempt{
		Epoch:    c.epoch,
		Keys:     c.keys,
		Model:    req.Model,
		Latency:  time.Since(started),
		ErrClass: inferenceErrClass(ctx, err),
	})

	return answers, err
}

// inferenceErrClass maps a Client.Evaluate outcome to its bounded class
// from typed sentinels only. Transport failures and non-2xx HTTP statuses
// share the unavailable class: the client folds both into one sentinel and
// they are indistinguishable without parsing error strings. Context
// sentinels survive the transport wrapping, so timeout and cancellation
// stay distinct.
func inferenceErrClass(_ context.Context, err error) statistics.InferenceErrClass {
	switch {
	case err == nil:
		return statistics.InferenceSuccess
	case errors.Is(err, context.DeadlineExceeded):
		return statistics.InferenceTimeout
	case errors.Is(err, context.Canceled):
		return statistics.InferenceCancelled
	case errors.Is(err, systemone.ErrAuth):
		return statistics.InferenceAuth
	case errors.Is(err, systemone.ErrUnavailable):
		return statistics.InferenceUnavailable
	case errors.Is(err, systemone.ErrMalformed):
		return statistics.InferenceInvalidResponse
	case errors.Is(err, systemone.ErrInvalidRequest):
		return statistics.InferencePretransportRejected
	case errors.Is(err, systemone.ErrNotConfigured):
		return statistics.InferenceNotConfigured
	default:
		// The client returns only typed sentinels; this is defensive.

		return statistics.InferenceUnavailable
	}
}

// decisionOutcome maps one Decide result to its bounded terminal
// decision-phase classification. The error chain is matched FIRST with
// errors.Is (the engine returns ReasonEvidenceInvalid for every
// pre-policy failure, so the reason alone cannot classify); the reason is
// consulted only for the nil-error cases.
func decisionOutcome(err error, decision interactivity.ActionDecision) statistics.InteractionOutcome {
	if err != nil {
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			return statistics.OutcomeBackendTimeout
		case errors.Is(err, context.Canceled):
			return statistics.OutcomeBackendCancelled
		case errors.Is(err, systemone.ErrAuth):
			return statistics.OutcomeBackendAuth
		case errors.Is(err, interactivity.ErrBackendUnavailable):
			return statistics.OutcomeBackendFailed
		case errors.Is(err, interactivity.ErrInvalidResponse):
			return statistics.OutcomeInvalidResponse
		case errors.Is(err, interactivity.ErrUnconfigured):
			return statistics.OutcomeUnconfigured
		case errors.Is(err, interactivity.ErrModelResolution),
			errors.Is(err, interactivity.ErrContextTooLarge),
			errors.Is(err, interactivity.ErrHistoryUnavailable),
			errors.Is(err, interactivity.ErrTargetUnavailable):
			// ErrHistoryUnavailable and ErrTargetUnavailable are unreachable
			// in production wiring; kept in the mapping defensively.
			return statistics.OutcomePreparationFailed
		default:
			// The engine returns typed errors only; this is defensive.

			return statistics.OutcomeBackendFailed
		}
	}

	switch decision.Reason {
	case interactivity.ReasonProfileDisabled:
		return statistics.OutcomeNoopProfileDisabled
	case interactivity.ReasonUnconfigured:
		return statistics.OutcomeUnconfigured
	case interactivity.ReasonIntrusionVeto:
		return statistics.OutcomeSelectedIgnoreVeto
	case interactivity.ReasonBelowThresholds:
		return statistics.OutcomeSelectedIgnoreBelow
	case interactivity.ReasonReact:
		return statistics.OutcomeSelectedReact
	case interactivity.ReasonReply:
		return statistics.OutcomeSelectedReply
	default:
		// ReasonEvidenceInvalid (and anything unknown) — the answers could
		// not be mapped to usable evidence.

		return statistics.OutcomeEvidenceInvalid
	}
}

// decisionScoresOf extracts the histogram input of a successful evaluation:
// the five component scores plus the policy base score recomputed through
// the exported interactivity.Base with the operation's profile (never a
// second implementation of the formula). Decisions that short-circuit
// before usable evidence — profile disabled, unconfigured, invalid
// evidence — record no scores.
func decisionScoresOf(profile interactivity.Profile, decision interactivity.ActionDecision) *statistics.ScoreSet {
	switch decision.Reason {
	case interactivity.ReasonIntrusionVeto, interactivity.ReasonBelowThresholds,
		interactivity.ReasonReact, interactivity.ReasonReply:
		return &statistics.ScoreSet{
			Base:             interactivity.Base(profile, decision.Scores),
			Relevance:        decision.Scores.Relevance,
			Contribution:     decision.Scores.Contribution,
			Intrusion:        decision.Scores.Intrusion,
			ReactSuitability: decision.Scores.ReactSuitable,
			ReplySuitability: decision.Scores.ReplySuitable,
		}
	default:
		return nil
	}
}

// profileOf resolves the built-in policy profile of an operation snapshot.
// Unknown values resolve to the zero profile — the engine treats unknown
// profiles as disabled, and score extraction never runs for those.
func profileOf(id adminconfig.InteractionProfile) interactivity.Profile {
	profile, _ := interactivity.ProfileByID(id)

	return profile
}

// execFailureOf maps the turn pipeline's failure classification onto the
// statistics enum. abandoned_target has no turn-level source — it stays a
// defensive-only enum member.
func execFailureOf(failure turnFailure) statistics.ExecFailure {
	switch failure {
	case turnFailureTimeout:
		return statistics.ExecFailureTimeout
	case turnFailureCancelled:
		return statistics.ExecFailureCancelled
	case turnFailureGeneration:
		return statistics.ExecFailureGeneration
	case turnFailureDelivery:
		return statistics.ExecFailureDelivery
	default:
		return statistics.ExecFailureNone
	}
}
