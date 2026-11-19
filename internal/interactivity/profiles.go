// Package interactivity implements the opt-in proactive participation
// decision (#157): an InteractionDecisionEngine evaluates eligible messages
// that did not produce an explicit turn against a System One decision
// backend and applies a deterministic policy over the built-in interaction
// profiles. The engine never selects or executes Telegram actions itself —
// handlers act on the returned ActionDecision.
package interactivity

import (
	"fmt"
	"math"

	"telegram-ollama-reply-bot/internal/adminconfig"
)

// Profile is one built-in interaction profile: a validated, immutable policy
// configuration. The five built-ins are the authoritative spec §5 table; the
// conformance test pins every field against it. Custom profiles are out of
// scope for MVP.
type Profile struct {
	// ID is the stable settings identifier (adminconfig.InteractionProfile).
	ID adminconfig.InteractionProfile
	// DisplayName is the human-readable name used in logs and screens.
	DisplayName string
	// Enabled gates the whole profile; the engine treats a disabled profile
	// as a NoOp before any resolution or HTTP.
	Enabled bool
	// AllowReact and AllowReply enable the two actions. Disabled actions are
	// disabled by flags, not by unreachable thresholds (1.0 stays reachable).
	AllowReact bool
	AllowReply bool

	// ReactionThreshold and ReplyThreshold are inclusive base-score gates.
	ReactionThreshold float64
	ReplyThreshold    float64
	// RelevanceWeight and ContributionWeight sum to 1.
	RelevanceWeight    float64
	ContributionWeight float64
	// IntrusionPenalty scales the intrusion probability subtracted from the
	// base score.
	IntrusionPenalty float64
	// ReactionSuitabilityMin and ReplySuitabilityMin are inclusive minimums
	// for the independent action-suitability probabilities.
	ReactionSuitabilityMin float64
	ReplySuitabilityMin    float64
	// MaxIntrusion is the hard veto: intrusion >= MaxIntrusion forces Ignore.
	MaxIntrusion float64
}

// Shared coefficients of all enabled built-ins (spec §5).
const (
	builtinRelevanceWeight        = 0.45
	builtinContributionWeight     = 0.55
	builtinReactionSuitabilityMin = 0.60
	builtinReplySuitabilityMin    = 0.70
	builtinMaxIntrusion           = 0.80
)

// Profiles returns the five built-in profiles in settings-enum order. The
// returned slice is a fresh copy; the profiles themselves are immutable by
// convention.
func Profiles() []Profile {
	return []Profile{
		{
			ID:          adminconfig.ProfileNonInteractive,
			DisplayName: "Non-interactive",
			Enabled:     false,
			AllowReact:  false,
			AllowReply:  false,
			// Thresholds stay finite so the data structure is total; a
			// disabled profile never consults them.
			ReactionThreshold:      1,
			ReplyThreshold:         1,
			RelevanceWeight:        builtinRelevanceWeight,
			ContributionWeight:     builtinContributionWeight,
			IntrusionPenalty:       0,
			ReactionSuitabilityMin: builtinReactionSuitabilityMin,
			ReplySuitabilityMin:    builtinReplySuitabilityMin,
			MaxIntrusion:           builtinMaxIntrusion,
		},
		{
			ID:                     adminconfig.ProfileReactionsLow,
			DisplayName:            "Reactions (low)",
			Enabled:                true,
			AllowReact:             true,
			AllowReply:             false,
			ReactionThreshold:      0.80,
			ReplyThreshold:         1,
			RelevanceWeight:        builtinRelevanceWeight,
			ContributionWeight:     builtinContributionWeight,
			IntrusionPenalty:       0.60,
			ReactionSuitabilityMin: builtinReactionSuitabilityMin,
			ReplySuitabilityMin:    builtinReplySuitabilityMin,
			MaxIntrusion:           builtinMaxIntrusion,
		},
		{
			ID:                     adminconfig.ProfileReactionsHigh,
			DisplayName:            "Reactions (high)",
			Enabled:                true,
			AllowReact:             true,
			AllowReply:             false,
			ReactionThreshold:      0.45,
			ReplyThreshold:         1,
			RelevanceWeight:        builtinRelevanceWeight,
			ContributionWeight:     builtinContributionWeight,
			IntrusionPenalty:       0.35,
			ReactionSuitabilityMin: builtinReactionSuitabilityMin,
			ReplySuitabilityMin:    builtinReplySuitabilityMin,
			MaxIntrusion:           builtinMaxIntrusion,
		},
		{
			ID:                     adminconfig.ProfileTalkLow,
			DisplayName:            "Talk (low)",
			Enabled:                true,
			AllowReact:             true,
			AllowReply:             true,
			ReactionThreshold:      0.50,
			ReplyThreshold:         0.90,
			RelevanceWeight:        builtinRelevanceWeight,
			ContributionWeight:     builtinContributionWeight,
			IntrusionPenalty:       0.50,
			ReactionSuitabilityMin: builtinReactionSuitabilityMin,
			ReplySuitabilityMin:    builtinReplySuitabilityMin,
			MaxIntrusion:           builtinMaxIntrusion,
		},
		{
			ID:                     adminconfig.ProfileTalkHigh,
			DisplayName:            "Talk (high)",
			Enabled:                true,
			AllowReact:             true,
			AllowReply:             true,
			ReactionThreshold:      0.30,
			ReplyThreshold:         0.65,
			RelevanceWeight:        builtinRelevanceWeight,
			ContributionWeight:     builtinContributionWeight,
			IntrusionPenalty:       0.30,
			ReactionSuitabilityMin: builtinReactionSuitabilityMin,
			ReplySuitabilityMin:    builtinReplySuitabilityMin,
			MaxIntrusion:           builtinMaxIntrusion,
		},
	}
}

// ProfileByID looks up a built-in profile by its settings identifier.
func ProfileByID(id adminconfig.InteractionProfile) (Profile, bool) {
	for _, profile := range Profiles() {
		if profile.ID == id {
			return profile, true
		}
	}

	return Profile{}, false
}

// ValidateProfile enforces the profile data invariants: every coefficient
// finite and within [0,1], weights summing to 1, enabled actions backed by
// flags, and when both actions are allowed an ordered threshold pair.
func ValidateProfile(profile Profile) error {
	finite01 := func(name string, value float64) error {
		if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > 1 {
			return fmt.Errorf("profile %s: %s must be finite within [0,1], got %v", profile.ID, name, value)
		}

		return nil
	}

	for _, check := range []struct {
		name  string
		value float64
	}{
		{"reaction_threshold", profile.ReactionThreshold},
		{"reply_threshold", profile.ReplyThreshold},
		{"relevance_weight", profile.RelevanceWeight},
		{"contribution_weight", profile.ContributionWeight},
		{"intrusion_penalty", profile.IntrusionPenalty},
		{"reaction_suitability_min", profile.ReactionSuitabilityMin},
		{"reply_suitability_min", profile.ReplySuitabilityMin},
		{"max_intrusion", profile.MaxIntrusion},
	} {
		if err := finite01(check.name, check.value); err != nil {
			return err
		}
	}

	const weightSumTolerance = 1e-9
	if sum := profile.RelevanceWeight + profile.ContributionWeight; math.Abs(sum-1) > weightSumTolerance {
		return fmt.Errorf("profile %s: relevance and contribution weights must sum to 1, got %v", profile.ID, sum)
	}

	if !profile.Enabled {
		if profile.AllowReact || profile.AllowReply {
			return fmt.Errorf("profile %s: a disabled profile must not allow actions", profile.ID)
		}

		return nil
	}

	if profile.AllowReact && !profile.AllowReply && profile.ReactionThreshold > profile.ReplyThreshold {
		// Reaction-only profiles have no upper reaction bound; their
		// reply threshold is the inert 1 sentinel, so this only fires on
		// inconsistent custom data.
		return fmt.Errorf("profile %s: reaction threshold must not exceed the inert reply bound", profile.ID)
	}
	if profile.AllowReact && profile.AllowReply && profile.ReactionThreshold > profile.ReplyThreshold {
		return fmt.Errorf("profile %s: reaction threshold must be <= reply threshold when both actions are allowed", profile.ID)
	}

	return nil
}
