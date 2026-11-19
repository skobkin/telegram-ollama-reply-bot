package interactivity

import (
	"math"
	"testing"

	"telegram-ollama-reply-bot/internal/adminconfig"
)

// TestBuiltInProfilesMatchSpec pins every field of the five built-in
// profiles against the authoritative spec §5 table. Any policy tuning must
// update this table deliberately.
func TestBuiltInProfilesMatchSpec(t *testing.T) {
	want := map[adminconfig.InteractionProfile]Profile{
		adminconfig.ProfileNonInteractive: {
			Enabled:                false,
			AllowReact:             false,
			AllowReply:             false,
			ReactionThreshold:      1,
			ReplyThreshold:         1,
			RelevanceWeight:        0.45,
			ContributionWeight:     0.55,
			IntrusionPenalty:       0,
			ReactionSuitabilityMin: 0.60,
			ReplySuitabilityMin:    0.70,
			MaxIntrusion:           0.80,
		},
		adminconfig.ProfileReactionsLow: {
			Enabled:                true,
			AllowReact:             true,
			AllowReply:             false,
			ReactionThreshold:      0.80,
			ReplyThreshold:         1,
			RelevanceWeight:        0.45,
			ContributionWeight:     0.55,
			IntrusionPenalty:       0.60,
			ReactionSuitabilityMin: 0.60,
			ReplySuitabilityMin:    0.70,
			MaxIntrusion:           0.80,
		},
		adminconfig.ProfileReactionsHigh: {
			Enabled:                true,
			AllowReact:             true,
			AllowReply:             false,
			ReactionThreshold:      0.45,
			ReplyThreshold:         1,
			RelevanceWeight:        0.45,
			ContributionWeight:     0.55,
			IntrusionPenalty:       0.35,
			ReactionSuitabilityMin: 0.60,
			ReplySuitabilityMin:    0.70,
			MaxIntrusion:           0.80,
		},
		adminconfig.ProfileTalkLow: {
			Enabled:                true,
			AllowReact:             true,
			AllowReply:             true,
			ReactionThreshold:      0.50,
			ReplyThreshold:         0.90,
			RelevanceWeight:        0.45,
			ContributionWeight:     0.55,
			IntrusionPenalty:       0.50,
			ReactionSuitabilityMin: 0.60,
			ReplySuitabilityMin:    0.70,
			MaxIntrusion:           0.80,
		},
		adminconfig.ProfileTalkHigh: {
			Enabled:                true,
			AllowReact:             true,
			AllowReply:             true,
			ReactionThreshold:      0.30,
			ReplyThreshold:         0.65,
			RelevanceWeight:        0.45,
			ContributionWeight:     0.55,
			IntrusionPenalty:       0.30,
			ReactionSuitabilityMin: 0.60,
			ReplySuitabilityMin:    0.70,
			MaxIntrusion:           0.80,
		},
	}

	builtins := Profiles()
	if len(builtins) != len(want) {
		t.Fatalf("expected %d built-in profiles, got %d", len(want), len(builtins))
	}
	for _, profile := range builtins {
		expected, ok := want[profile.ID]
		if !ok {
			t.Fatalf("unexpected built-in profile %q", profile.ID)
		}
		// Compare only the spec-table fields; identity and display name are
		// checked separately below.
		got := profile
		got.ID, got.DisplayName = "", ""
		if got != expected {
			t.Fatalf("profile %q drifted from the spec table:\n got %+v\nwant %+v", profile.ID, profile, expected)
		}
		if profile.DisplayName == "" {
			t.Fatalf("profile %q must carry a display name", profile.ID)
		}
		if err := ValidateProfile(profile); err != nil {
			t.Fatalf("built-in profile %q must validate: %v", profile.ID, err)
		}
	}

	// Every settings enum value resolves to a built-in and vice versa.
	for _, id := range adminconfig.InteractionProfiles() {
		if _, ok := ProfileByID(id); !ok {
			t.Fatalf("settings enum value %q has no built-in profile", id)
		}
	}
	if _, ok := ProfileByID("nonexistent"); ok {
		t.Fatalf("unknown profile id must not resolve")
	}
}

// TestValidateProfileRejectsInvalidData pins the profile data invariants.
func TestValidateProfileRejectsInvalidData(t *testing.T) {
	valid := Profile{
		ID:                     "custom",
		Enabled:                true,
		AllowReact:             true,
		AllowReply:             true,
		ReactionThreshold:      0.3,
		ReplyThreshold:         0.6,
		RelevanceWeight:        0.45,
		ContributionWeight:     0.55,
		IntrusionPenalty:       0.3,
		ReactionSuitabilityMin: 0.6,
		ReplySuitabilityMin:    0.7,
		MaxIntrusion:           0.8,
	}
	if err := ValidateProfile(valid); err != nil {
		t.Fatalf("valid profile must pass: %v", err)
	}

	cases := map[string]func(p Profile) Profile{
		"weights do not sum to 1": func(p Profile) Profile {
			p.ContributionWeight = 0.5

			return p
		},
		"NaN threshold": func(p Profile) Profile {
			p.ReactionThreshold = math.NaN()

			return p
		},
		"inf penalty": func(p Profile) Profile {
			p.IntrusionPenalty = math.Inf(1)

			return p
		},
		"negative suitability": func(p Profile) Profile {
			p.ReplySuitabilityMin = -0.1

			return p
		},
		"veto above 1": func(p Profile) Profile {
			p.MaxIntrusion = 1.5

			return p
		},
		"reaction threshold above reply threshold": func(p Profile) Profile {
			p.ReactionThreshold = 0.7

			return p
		},
	}
	for name, mutate := range cases {
		if err := ValidateProfile(mutate(valid)); err == nil {
			t.Fatalf("%s must be rejected", name)
		}
	}

	// A disabled profile must not allow actions.
	disabled := valid
	disabled.Enabled = false
	disabled.AllowReact = true
	if err := ValidateProfile(disabled); err == nil {
		t.Fatalf("disabled profile allowing actions must be rejected")
	}

	// A reaction-only profile needs no reply-threshold ordering: its reply
	// bound is the inert 1 sentinel.
	reactionOnly := valid
	reactionOnly.AllowReply = false
	reactionOnly.ReplyThreshold = 1
	reactionOnly.ReactionThreshold = 0.8
	if err := ValidateProfile(reactionOnly); err != nil {
		t.Fatalf("reaction-only profile must validate: %v", err)
	}
}
