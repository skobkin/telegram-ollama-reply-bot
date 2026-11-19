package statistics

import (
	"encoding/json"
	"fmt"
	"time"
)

// Schema and version of the interaction-statistics export. Any shape change
// bumps the version.
const (
	InteractionSchemaName    = "tg-llm-bot/interaction-statistics"
	InteractionSchemaVersion = 1
)

// GeneralJSON renders the legacy pretty-JSON representation of a general
// snapshot — the /stats command and admin-screen format. This is a
// presenter, not a collector method: callers snapshot first, then render.
// The output is byte-identical to the pre-consolidation StatsSnapshot
// string form (same struct tags, same field order, same marshal settings).
func GeneralJSON(s GeneralSnapshot) string {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return "{\"error\": \"cannot serialize stats\"}"
	}

	return string(data)
}

// InteractionJSON renders the versioned interaction-statistics export for
// one snapshot with the scope and filters it was taken under (the snapshot
// itself carries only the narrowed VIEW; the envelope documents the
// selection). The output is self-describing: histogram edges, percentile
// semantics, evaluated-definition and overflow disclosures are embedded, so
// the document never depends on out-of-band knowledge.
func InteractionJSON(snapshot InteractionSnapshot, scope InteractionScope, filters InteractionFilters) ([]byte, error) {
	envelope := interactionExport{
		Schema:            InteractionSchemaName,
		Version:           InteractionSchemaVersion,
		GeneratedAt:       snapshot.GeneratedAt,
		Mode:              snapshot.Mode,
		RubricVersion:     snapshot.RubricVersion,
		ObservationPeriod: snapshot.Period,
		Scope:             exportScopeOf(scope),
		FiltersApplied:    exportFiltersOf(filters),
		Submission:        snapshot.Submission,
		Preflight:         snapshot.Preflight,
		Decision:          snapshot.Decision,
		Execution:         snapshot.Execution,
		Notes:             exportNotesOf(snapshot),
	}

	data, err := json.MarshalIndent(envelope, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("serialize interaction export: %w", err)
	}

	return data, nil
}

// interactionExport is the versioned envelope around the snapshot sections.
type interactionExport struct {
	Schema            string            `json:"schema"`
	Version           int               `json:"version"`
	GeneratedAt       time.Time         `json:"generated_at"`
	Mode              string            `json:"mode"`
	RubricVersion     int               `json:"rubric_version"`
	ObservationPeriod InteractionPeriod `json:"observation_period"`
	Scope             exportScope       `json:"scope"`
	FiltersApplied    exportFilters     `json:"filters_applied"`

	// Submission and Preflight are nil when profile/model filters omitted
	// them (chat-keyed denominators are not filterable); the notes say so.
	Submission *SubmissionTotals `json:"submission"`
	Preflight  *PreflightSection `json:"preflight"`

	Decision  DecisionSection  `json:"decision"`
	Execution ExecutionSection `json:"execution"`

	Notes exportNotes `json:"notes"`
}

// exportScope documents the selection the snapshot was taken under.
type exportScope struct {
	Kind   string `json:"kind"` // "global" or "chat"
	ChatID *int64 `json:"chat_id"`
}

func exportScopeOf(scope InteractionScope) exportScope {
	if !scope.ChatSpecified {
		return exportScope{Kind: "global"}
	}

	chatID := scope.ChatID

	return exportScope{Kind: "chat", ChatID: &chatID}
}

// exportFilters documents the active bucket filters (empty = unfiltered).
type exportFilters struct {
	Profile       string `json:"profile,omitempty"`
	DecisionModel string `json:"decision_model,omitempty"`
}

func exportFiltersOf(filters InteractionFilters) exportFilters {
	// Structural conversion: the field sets are identical by design, and a
	// new filter field breaks this line until the export documents it.
	return exportFilters(filters)
}

// exportNotes carries the reading instructions and honesty disclosures.
type exportNotes struct {
	Percentiles        string `json:"percentiles"`
	ChatOverflowCounts uint64 `json:"chat_overflow_counts"`
	// ModelOverflowCounts is the number of decision-model overflow buckets
	// in the export (decision section and execution decision_model
	// dimension).
	ModelOverflowCounts uint64 `json:"model_overflow_counts"`
	// GenerationModelOverflowCounts covers the execution generation_model
	// dimension.
	GenerationModelOverflowCounts uint64 `json:"generation_model_overflow_counts"`
	// OversizedIDsOverflowed reports that at least one observation was
	// folded into overflow because its model identifier exceeded the size
	// bound (folded WITHOUT truncation — never kept as a truncated key).
	OversizedIDsOverflowed bool `json:"oversized_ids_overflowed"`
	// AttributionIncomplete is true when overflow buckets are present in
	// this view: per-chat/per-model attribution is incomplete because
	// folded observations cannot be attributed.
	AttributionIncomplete bool `json:"attribution_incomplete"`
	// FilteredTotalsExact is false when the view is narrowed while overflow
	// folding is active: exact global totals never imply exact filtered
	// totals.
	FilteredTotalsExact bool     `json:"filtered_totals_exact"`
	UntrackedSelection  *string  `json:"untracked_selection"`
	EvaluatedDefinition string   `json:"evaluated_definition"`
	OmittedUnderFilters []string `json:"omitted_under_filters"`
}

func exportNotesOf(snapshot InteractionSnapshot) exportNotes {
	notes := exportNotes{
		Percentiles:         "percentiles are histogram approximations; at_least_ms is the terminal open-bin lower bound; null = no data",
		FilteredTotalsExact: snapshot.FilteredTotalsExact,
		EvaluatedDefinition: "Client.Evaluate invocations including pre-transport rejections; NoOp, unconfigured and pre-inference failures are decision outcomes, not evaluated attempts; no exact HTTP-request count is claimed",
		OmittedUnderFilters: []string{},
	}
	if snapshot.UntrackedSelection != "" {
		selection := snapshot.UntrackedSelection
		notes.UntrackedSelection = &selection
	}

	if snapshot.Submission == nil {
		notes.OmittedUnderFilters = append(notes.OmittedUnderFilters, "submission")
	}
	if snapshot.Preflight == nil {
		notes.OmittedUnderFilters = append(notes.OmittedUnderFilters, "preflight")
	}

	// A nil section is omitted under filters; its overflow buckets are not
	// part of this view and contribute nothing to the counts.
	if snapshot.Preflight != nil {
		for _, chat := range snapshot.Preflight.Chats {
			if chat.ChatOverflow {
				notes.ChatOverflowCounts++
			}
		}
	}
	for _, bucket := range snapshot.Decision.Buckets {
		if bucket.ChatOverflow {
			notes.ChatOverflowCounts++
		}
		if bucket.DecisionModelOverflow {
			notes.ModelOverflowCounts++
		}
	}
	for _, bucket := range snapshot.Execution.Buckets {
		if bucket.ChatOverflow {
			notes.ChatOverflowCounts++
		}
		if bucket.DecisionModelOverflow {
			notes.ModelOverflowCounts++
		}
		if bucket.GenerationModelOverflow {
			notes.GenerationModelOverflowCounts++
		}
	}

	notes.OversizedIDsOverflowed = snapshot.OversizedIDFolds > 0
	notes.AttributionIncomplete = notes.ChatOverflowCounts > 0 ||
		notes.ModelOverflowCounts > 0 ||
		notes.GenerationModelOverflowCounts > 0

	return notes
}
