package statistics

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// fillInteractionCollector records one of everything so shape tests exercise
// every section of the export.
func fillInteractionCollector(t *testing.T, rubric int) *Collector {
	t.Helper()

	c := NewCollector(Config{RubricVersion: rubric})

	epoch := c.BeginSubmission(100)
	c.RecordPreflight(PreflightObservation{Epoch: epoch, ChatID: 100, Kind: PreflightAccessDenied})
	c.RecordInference(InferenceAttempt{
		Epoch: epoch, Keys: ik(100, "talk_low", "m1"), Model: "m1",
		Latency: 800 * time.Millisecond, ErrClass: InferenceSuccess,
	})
	c.RecordDecision(DecisionObservation{
		Epoch: epoch, Keys: ik(100, "talk_low", "m1"), Outcome: OutcomeSelectedReply,
		Scores: scores(0.4), DecisionLatency: 1500 * time.Millisecond,
	})
	c.RecordExecutionDispatch(ExecutionDispatch{
		Epoch: epoch, Keys: ik(100, "talk_low", "m1"), GenerationModel: "gen-model", Action: ActionReply,
	})
	c.RecordExecution(ExecutionObservation{
		Epoch: epoch, Keys: ik(100, "talk_low", "m1"), GenerationModel: "gen-model", Action: ActionReply,
		TextConfirmed: true, Duration: 2 * time.Second,
	})

	return c
}

// mustInteractionJSON renders the export or fails the test.
func mustInteractionJSON(t *testing.T, c *Collector, scope InteractionScope, filters InteractionFilters) ([]byte, interactionExport) {
	t.Helper()

	snap := c.InteractionSnapshot(scope, filters)
	data, err := InteractionJSON(snap, scope, filters)
	if err != nil {
		t.Fatalf("InteractionJSON: %v", err)
	}

	var decoded interactionExport
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("export must round-trip into its envelope struct: %v", err)
	}

	return data, decoded
}

func TestInteractionExportShapeAndRoundTrip(t *testing.T) {
	c := fillInteractionCollector(t, 7)
	data, decoded := mustInteractionJSON(t, c, GlobalScope(), InteractionFilters{})

	if decoded.Schema != InteractionSchemaName {
		t.Fatalf("schema = %q", decoded.Schema)
	}
	if decoded.Version != InteractionSchemaVersion {
		t.Fatalf("version = %d, want %d", decoded.Version, InteractionSchemaVersion)
	}
	if decoded.Mode != ModeLive {
		t.Fatalf("mode = %q, want %q", decoded.Mode, ModeLive)
	}
	if decoded.RubricVersion != 7 {
		t.Fatalf("rubric_version = %d, want 7", decoded.RubricVersion)
	}
	if decoded.Scope.Kind != "global" || decoded.Scope.ChatID != nil {
		t.Fatalf("global scope must render kind=global with null chat_id: %+v", decoded.Scope)
	}
	if decoded.FiltersApplied.Profile != "" || decoded.FiltersApplied.DecisionModel != "" {
		t.Fatalf("no filters must render empty filters_applied: %+v", decoded.FiltersApplied)
	}
	if decoded.Submission == nil || decoded.Submission.Submitted != 1 {
		t.Fatalf("submission section missing or wrong: %+v", decoded.Submission)
	}
	if decoded.Preflight == nil || decoded.Preflight.Totals.AccessDenied != 1 {
		t.Fatalf("preflight section missing or wrong: %+v", decoded.Preflight)
	}
	if got := decoded.Decision.Totals.Outcomes.SelectedReply; got != 1 {
		t.Fatalf("decision outcomes selected_reply = %d, want 1", got)
	}
	if got := decoded.Decision.Totals.EvaluatedAttempts; got != 1 {
		t.Fatalf("evaluated_attempts = %d, want 1", got)
	}
	if got := decoded.Execution.Totals.TextsConfirmed; got != 1 {
		t.Fatalf("texts_confirmed = %d, want 1", got)
	}

	// Key sets are pinned: shape stability and a privacy ceiling (no field
	// where conversation text could hide goes unnoticed).
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("decode raw: %v", err)
	}
	wantTop := []string{
		"schema", "version", "generated_at", "mode", "rubric_version",
		"observation_period", "scope", "filters_applied", "submission",
		"preflight", "decision", "execution", "notes",
	}
	if len(raw) != len(wantTop) {
		t.Fatalf("top-level keys = %d, want %d: %v", len(raw), len(wantTop), raw)
	}
	for _, key := range wantTop {
		if _, ok := raw[key]; !ok {
			t.Fatalf("top-level key %q missing", key)
		}
	}

	var notes map[string]json.RawMessage
	if err := json.Unmarshal(raw["notes"], &notes); err != nil {
		t.Fatalf("decode notes: %v", err)
	}
	wantNotes := []string{
		"percentiles", "chat_overflow_counts", "model_overflow_counts",
		"generation_model_overflow_counts", "oversized_ids_overflowed",
		"attribution_incomplete", "filtered_totals_exact",
		"untracked_selection", "evaluated_definition", "omitted_under_filters",
	}
	if len(notes) != len(wantNotes) {
		t.Fatalf("notes keys = %d, want %d: %v", len(notes), len(wantNotes), notes)
	}
	for _, key := range wantNotes {
		if _, ok := notes[key]; !ok {
			t.Fatalf("notes key %q missing", key)
		}
	}

	// Histograms are self-describing: embedded edges and the open-ended
	// terminal bin flag.
	if len(decoded.Decision.Totals.ScoreHistograms.Base.Edges) != scoreBinCount+1 {
		t.Fatalf("score histogram must embed its edges, got %d", len(decoded.Decision.Totals.ScoreHistograms.Base.Edges))
	}
	if !decoded.Decision.Totals.Latency.Decision.OpenEnded {
		t.Fatalf("latency histogram must declare its open-ended terminal bin")
	}
	// The fixture recorded decision latency, so percentiles are populated
	// approximations (a fresh collector reports null — asserted below).
	if p50 := decoded.Decision.Totals.Latency.Percentiles.Decision.P50; p50 == nil || p50.ApproxMs == nil {
		t.Fatalf("recorded latency must yield approx percentiles, got %+v", p50)
	}

	// A clean view is exact and complete.
	if !decoded.Notes.FilteredTotalsExact {
		t.Fatalf("unfiltered view must be filtered_totals_exact")
	}
	if decoded.Notes.AttributionIncomplete {
		t.Fatalf("clean view must not claim attribution_incomplete")
	}
	if decoded.Notes.OversizedIDsOverflowed {
		t.Fatalf("clean view must not claim oversized ids")
	}
	if len(decoded.Notes.OmittedUnderFilters) != 0 {
		t.Fatalf("unfiltered view omits nothing, got %v", decoded.Notes.OmittedUnderFilters)
	}
	if decoded.Notes.UntrackedSelection != nil {
		t.Fatalf("clean view must not set untracked_selection")
	}
}

func TestInteractionExportChatScope(t *testing.T) {
	c := fillInteractionCollector(t, 1)
	other := c.BeginSubmission(200)
	c.RecordDecision(DecisionObservation{
		Epoch: other, Keys: ik(200, "talk_low", "m1"), Outcome: OutcomeSelectedIgnoreBelow,
		Scores: scores(0.2), DecisionLatency: time.Second,
	})

	_, decoded := mustInteractionJSON(t, c, ChatScope(100), InteractionFilters{})
	if decoded.Scope.Kind != "chat" || decoded.Scope.ChatID == nil || *decoded.Scope.ChatID != 100 {
		t.Fatalf("chat scope must render kind=chat with the chat_id: %+v", decoded.Scope)
	}
	// Chat scope keeps the chat-keyed sections and stays scoped: the other
	// chat's outcome must not leak in.
	if got := decoded.Decision.Totals.Outcomes.SelectedIgnoreBelow; got != 0 {
		t.Fatalf("chat scope must not include other chats, got %d", got)
	}
	if got := decoded.Decision.Totals.Outcomes.SelectedReply; got != 1 {
		t.Fatalf("chat scope must include the selected chat, got %d", got)
	}
	if len(decoded.Notes.OmittedUnderFilters) != 0 {
		t.Fatalf("chat scope without filters keeps everything: %v", decoded.Notes.OmittedUnderFilters)
	}
}

func TestInteractionExportFiltersOmitChatKeyedSections(t *testing.T) {
	c := fillInteractionCollector(t, 1)

	_, decoded := mustInteractionJSON(t, c, GlobalScope(), InteractionFilters{
		Profile: "talk_low", DecisionModel: "m1",
	})
	if decoded.FiltersApplied.Profile != "talk_low" || decoded.FiltersApplied.DecisionModel != "m1" {
		t.Fatalf("filters_applied must document the active filters: %+v", decoded.FiltersApplied)
	}
	if decoded.Submission != nil || decoded.Preflight != nil {
		t.Fatalf("profile/model filters must omit the chat-keyed sections")
	}
	if len(decoded.Notes.OmittedUnderFilters) != 2 {
		t.Fatalf("omitted_under_filters = %v, want both sections", decoded.Notes.OmittedUnderFilters)
	}
	if decoded.Notes.OmittedUnderFilters[0] != "submission" || decoded.Notes.OmittedUnderFilters[1] != "preflight" {
		t.Fatalf("omitted_under_filters = %v, want [submission preflight]", decoded.Notes.OmittedUnderFilters)
	}
	if got := decoded.Decision.Totals.Outcomes.SelectedReply; got != 1 {
		t.Fatalf("matching filter keeps the outcome: %d", got)
	}
}

func TestInteractionExportOverflowDisclosure(t *testing.T) {
	c := NewCollector(Config{RubricVersion: 1})

	// Fill the chat cap, then fold three more chats into overflow.
	for chat := int64(1); chat <= MaxTrackedChats+3; chat++ {
		epoch := c.BeginSubmission(chat)
		c.RecordDecision(DecisionObservation{
			Epoch: epoch, Keys: ik(chat, "talk_low", "m1"),
			Outcome: OutcomeSelectedIgnoreBelow, Scores: scores(0.2), DecisionLatency: time.Second,
		})
	}
	// One oversized model identifier folds without truncation.
	oversized := strings.Repeat("long-model-", 12) + "tail" // > 128 bytes
	epoch := c.BeginSubmission(1)
	c.RecordInference(InferenceAttempt{
		Epoch: epoch, Keys: ik(1, "talk_low", oversized), Model: oversized,
		Latency: time.Second, ErrClass: InferenceUnavailable,
	})

	// Global unfiltered view: overflow is disclosed, totals stay exact.
	_, decoded := mustInteractionJSON(t, c, GlobalScope(), InteractionFilters{})
	if !decoded.Notes.AttributionIncomplete {
		t.Fatalf("overflow present must set attribution_incomplete")
	}
	if decoded.Notes.ChatOverflowCounts < 1 {
		t.Fatalf("folded chats must disclose chat overflow buckets, got %d", decoded.Notes.ChatOverflowCounts)
	}
	if !decoded.Notes.OversizedIDsOverflowed {
		t.Fatalf("oversized ID folds must be disclosed")
	}
	if decoded.Notes.FilteredTotalsExact != true {
		t.Fatalf("unfiltered global totals stay exact, got %t", decoded.Notes.FilteredTotalsExact)
	}
	if decoded.Notes.UntrackedSelection != nil {
		t.Fatalf("global view is never untracked")
	}

	// Narrowed view under overflow: filtered totals are no longer exact.
	_, decoded = mustInteractionJSON(t, c, GlobalScope(), InteractionFilters{Profile: "talk_low"})
	if decoded.Notes.FilteredTotalsExact {
		t.Fatalf("narrowed view under overflow must set filtered_totals_exact=false")
	}
	if !decoded.Notes.AttributionIncomplete {
		t.Fatalf("overflow buckets remain in the narrowed view")
	}

	// A selection matching nothing tracked says so instead of showing zeros.
	_, decoded = mustInteractionJSON(t, c, GlobalScope(), InteractionFilters{DecisionModel: "never-seen-model"})
	if decoded.Notes.UntrackedSelection == nil {
		t.Fatalf("untracked selection must be disclosed, not presented as zeros")
	}

	// A model literally named "overflow" is an ordinary ID: it is tracked,
	// filterable, and never counted into the overflow disclosures.
	c2 := NewCollector(Config{RubricVersion: 1})
	epoch2 := c2.BeginSubmission(50)
	c2.RecordDecision(DecisionObservation{
		Epoch: epoch2, Keys: ik(50, "talk_low", "overflow"), Outcome: OutcomeSelectedReact,
		Scores: scores(0.5), DecisionLatency: time.Second,
	})

	_, decoded = mustInteractionJSON(t, c2, GlobalScope(), InteractionFilters{})
	if decoded.Notes.AttributionIncomplete || decoded.Notes.ModelOverflowCounts != 0 {
		t.Fatalf("a model named \"overflow\" must not trip the overflow disclosure: %+v", decoded.Notes)
	}
	for _, bucket := range decoded.Decision.Buckets {
		if bucket.DecisionModel == nil || *bucket.DecisionModel != "overflow" || bucket.DecisionModelOverflow {
			t.Fatalf("model named \"overflow\" must render as a normal ID: %+v", bucket)
		}
	}

	_, decoded = mustInteractionJSON(t, c2, GlobalScope(), InteractionFilters{DecisionModel: "overflow"})
	if got := decoded.Decision.Totals.Outcomes.SelectedReact; got != 1 {
		t.Fatalf("filter by the literal name \"overflow\" must match the real model, got %d", got)
	}
	if decoded.Notes.UntrackedSelection != nil {
		t.Fatalf("a tracked model named \"overflow\" is not an untracked selection")
	}
}

func TestInteractionExportNoInfNaN(t *testing.T) {
	c := NewCollector(Config{RubricVersion: 1})

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
		Epoch: epoch, Keys: ik(400, "talk_low", "m1"), GenerationModel: "gen", Action: ActionReply,
		Duration: 10 * time.Hour,
	})

	data, _ := mustInteractionJSON(t, c, GlobalScope(), InteractionFilters{})
	text := string(data)
	for _, bad := range []string{"Inf", "NaN"} {
		if strings.Contains(text, bad) {
			t.Fatalf("export contains %q; +Inf/NaN must never reach the encoder", bad)
		}
	}
}
