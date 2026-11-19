package pngreport

import (
	"bytes"
	"image"
	_image "image/png"
	"testing"
	"time"

	"telegram-ollama-reply-bot/internal/statistics"
)

// newSeededCollector records a representative observation set across two
// chats, two profiles and two decision models: a preflight skip, vetoes,
// positive selections with varied scores and latencies (including one past
// the open-ended terminal bin), execution effects and failure classes.
func newSeededCollector(t *testing.T) *statistics.Collector {
	t.Helper()

	c := statistics.NewCollector(statistics.Config{RubricVersion: 7})
	keys := func(chat int64, profile, model string) statistics.InteractionKeys {
		return statistics.InteractionKeys{ChatID: chat, Profile: profile, DecisionModel: model, Mode: statistics.ModeLive}
	}

	// Chat 100 / talk_low: one preflight skip, then a reply turn.
	epoch := c.BeginSubmission(100)
	c.RecordPreflight(statistics.PreflightObservation{Epoch: epoch, ChatID: 100, Kind: statistics.PreflightAccessDenied})

	epoch = c.BeginSubmission(100)
	c.RecordInference(statistics.InferenceAttempt{
		Epoch: epoch, Keys: keys(100, "talk_low", "m1"), Model: "m1",
		Latency: 700 * time.Millisecond, ErrClass: statistics.InferenceSuccess,
	})
	c.RecordDecision(statistics.DecisionObservation{
		Epoch: epoch, Keys: keys(100, "talk_low", "m1"), Outcome: statistics.OutcomeSelectedReply,
		Scores:          &statistics.ScoreSet{Base: 0.42, Relevance: 0.6, Contribution: 0.55, Intrusion: 0.2, ReactSuitability: 0.5, ReplySuitability: 0.75},
		DecisionLatency: 1400 * time.Millisecond,
	})
	c.RecordExecutionDispatch(statistics.ExecutionDispatch{
		Epoch: epoch, Keys: keys(100, "talk_low", "m1"), GenerationModel: "gen-a", Action: statistics.ActionReply,
	})
	c.RecordExecution(statistics.ExecutionObservation{
		Epoch: epoch, Keys: keys(100, "talk_low", "m1"), GenerationModel: "gen-a", Action: statistics.ActionReply,
		TextConfirmed: true, Duration: 2100 * time.Millisecond,
	})

	// Partial success: reaction confirmed but generation failed afterwards.
	epoch = c.BeginSubmission(100)
	c.RecordInference(statistics.InferenceAttempt{
		Epoch: epoch, Keys: keys(100, "talk_low", "m1"), Model: "m1",
		Latency: 300 * time.Millisecond, ErrClass: statistics.InferenceSuccess,
	})
	c.RecordDecision(statistics.DecisionObservation{
		Epoch: epoch, Keys: keys(100, "talk_low", "m1"), Outcome: statistics.OutcomeSelectedReact,
		Scores:          &statistics.ScoreSet{Base: 0.55, Relevance: 0.7, Contribution: 0.5, Intrusion: 0.1, ReactSuitability: 0.8, ReplySuitability: 0.3},
		DecisionLatency: 900 * time.Millisecond,
	})
	c.RecordExecutionDispatch(statistics.ExecutionDispatch{
		Epoch: epoch, Keys: keys(100, "talk_low", "m1"), GenerationModel: "gen-a", Action: statistics.ActionReact,
	})
	c.RecordExecution(statistics.ExecutionObservation{
		Epoch: epoch, Keys: keys(100, "talk_low", "m1"), GenerationModel: "gen-a", Action: statistics.ActionReact,
		ReactionConfirmed: true, Failure: statistics.ExecFailureGeneration, Duration: 4800 * time.Millisecond,
	})

	// Chat 200 / reactions_high: a veto without execution.
	epoch = c.BeginSubmission(200)
	c.RecordInference(statistics.InferenceAttempt{
		Epoch: epoch, Keys: keys(200, "reactions_high", "m2"), Model: "m2",
		Latency: 900 * time.Millisecond, ErrClass: statistics.InferenceSuccess,
	})
	c.RecordDecision(statistics.DecisionObservation{
		Epoch: epoch, Keys: keys(200, "reactions_high", "m2"), Outcome: statistics.OutcomeSelectedIgnoreVeto,
		Scores:          &statistics.ScoreSet{Base: 0.1, Relevance: 0.3, Contribution: 0.2, Intrusion: 0.95, ReactSuitability: 0.4, ReplySuitability: 0.2},
		DecisionLatency: 2500 * time.Millisecond,
	})

	// A backend timeout: inference times out, the outcome classifies it.
	epoch = c.BeginSubmission(200)
	c.RecordInference(statistics.InferenceAttempt{
		Epoch: epoch, Keys: keys(200, "reactions_high", "m2"), Model: "m2",
		Latency: 31 * time.Second, ErrClass: statistics.InferenceTimeout,
	})
	c.RecordDecision(statistics.DecisionObservation{
		Epoch: epoch, Keys: keys(200, "reactions_high", "m2"), Outcome: statistics.OutcomeBackendTimeout,
		DecisionLatency: 31 * time.Second,
	})

	// A long-tail execution landing in the open-ended terminal bin, with a
	// confirmed text AND another confirmed side effect (turn flags co-occur).
	epoch = c.BeginSubmission(200)
	c.RecordInference(statistics.InferenceAttempt{
		Epoch: epoch, Keys: keys(200, "reactions_high", "m1"), Model: "m1",
		Latency: 1100 * time.Millisecond, ErrClass: statistics.InferenceSuccess,
	})
	c.RecordDecision(statistics.DecisionObservation{
		Epoch: epoch, Keys: keys(200, "reactions_high", "m1"), Outcome: statistics.OutcomeSelectedReply,
		Scores:          &statistics.ScoreSet{Base: 0.63, Relevance: 0.85, Contribution: 0.7, Intrusion: 0.15, ReactSuitability: 0.6, ReplySuitability: 0.9},
		DecisionLatency: 1800 * time.Millisecond,
	})
	c.RecordExecutionDispatch(statistics.ExecutionDispatch{
		Epoch: epoch, Keys: keys(200, "reactions_high", "m1"), GenerationModel: "gen-c", Action: statistics.ActionReply,
	})
	c.RecordExecution(statistics.ExecutionObservation{
		Epoch: epoch, Keys: keys(200, "reactions_high", "m1"), GenerationModel: "gen-c", Action: statistics.ActionReply,
		TextConfirmed: true, OtherSideEffectsConfirmed: true,
		Duration: 130 * time.Second,
	})

	return c
}

// seededReport renders the seeded collector's global snapshot.
func seededReport(t *testing.T, c *statistics.Collector) Report {
	t.Helper()

	scope, filters := statistics.GlobalScope(), statistics.InteractionFilters{}

	return Report{
		Snapshot: c.InteractionSnapshot(scope, filters),
		Scope:    scope,
		Filters:  filters,
	}
}

// renderReport renders a report or fails the test.
func renderReport(t *testing.T, input Report) []byte {
	t.Helper()

	data, err := Render(input)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	return data
}

// decodeReport renders a report and decodes the PNG, returning the image.
func decodeReport(t *testing.T, input Report) image.Image {
	t.Helper()

	data := renderReport(t, input)
	img, err := _image.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("report must decode as a PNG: %v", err)
	}
	if got := img.Bounds(); got.Dx() != reportWidth || got.Dy() != reportHeight {
		t.Fatalf("report bounds = %dx%d, want %dx%d", got.Dx(), got.Dy(), reportWidth, reportHeight)
	}

	return img
}

func TestRenderProducesDecodablePNG(t *testing.T) {
	c := newSeededCollector(t)

	img := decodeReport(t, seededReport(t, c))
	if img == nil {
		t.Fatalf("decoded image is nil")
	}

	// Non-trivial payload: a mostly-flat canvas of this size compresses far
	// below this bound; real panels with charts do not.
	data := renderReport(t, seededReport(t, c))
	if len(data) < 20_000 {
		t.Fatalf("report render is suspiciously small: %d bytes", len(data))
	}
}

func TestRenderEmptyState(t *testing.T) {
	c := statistics.NewCollector(statistics.Config{RubricVersion: 7})

	// A fresh collector renders the explicit empty state: decodable, and
	// different from a populated report.
	decodeReport(t, seededReport(t, c))

	filled := renderReport(t, seededReport(t, newSeededCollector(t)))
	emptyBytes := renderReport(t, seededReport(t, c))
	if bytes.Equal(emptyBytes, filled) {
		t.Fatalf("the empty state must not render like a populated report")
	}
}

func TestRenderAnnotationsChangeOutput(t *testing.T) {
	c := newSeededCollector(t)

	// Single-profile view without annotations (multi-profile variant).
	scope, filters := statistics.GlobalScope(), statistics.InteractionFilters{Profile: "talk_low"}
	base := Report{Snapshot: c.InteractionSnapshot(scope, filters), Scope: scope, Filters: filters}
	plain, err := Render(base)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	// With resolved thresholds the report gains the threshold ruler.
	annotated := base
	annotated.Annotations = &ThresholdAnnotations{
		ProfileID:         "talk_low",
		ReactionThreshold: 0.6,
		ReplyThreshold:    0.7,
		MaxIntrusion:      0.8,
	}
	marked, err := Render(annotated)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if bytes.Equal(plain, marked) {
		t.Fatalf("annotations must change the rendering (threshold ruler present)")
	}

	// Both variants decode.
	decodeReport(t, base)
	decodeReport(t, annotated)
}

func TestRenderDisclosesFoldedTotals(t *testing.T) {
	c := newSeededCollector(t)

	// Fill the chat cap so overflow folding becomes active, then narrow the
	// view with a profile filter: the snapshot must carry the disclosure
	// flags and the report must render differently (the banner).
	for chat := int64(300); chat <= int64(300+statistics.MaxTrackedChats); chat++ {
		epoch := c.BeginSubmission(chat)
		c.RecordInference(statistics.InferenceAttempt{
			Epoch: epoch, Keys: statistics.InteractionKeys{ChatID: chat, Profile: "talk_low", DecisionModel: "m1", Mode: statistics.ModeLive},
			Model: "m1", Latency: 100 * time.Millisecond, ErrClass: statistics.InferenceSuccess,
		})
		c.RecordDecision(statistics.DecisionObservation{
			Epoch: epoch, Keys: statistics.InteractionKeys{ChatID: chat, Profile: "talk_low", DecisionModel: "m1", Mode: statistics.ModeLive},
			Outcome: statistics.OutcomeSelectedIgnoreBelow,
		})
	}

	scope, filters := statistics.GlobalScope(), statistics.InteractionFilters{Profile: "talk_low"}
	snapshot := c.InteractionSnapshot(scope, filters)
	if snapshot.FilteredTotalsExact {
		t.Fatalf("a narrowed view under overflow must set filtered_totals_exact=false")
	}
	clean := renderReport(t, seededReport(t, c))

	banner := Report{Snapshot: snapshot, Scope: scope, Filters: filters}
	flagged := renderReport(t, banner)
	if bytes.Equal(flagged, clean) {
		t.Fatalf("a folded filtered view must render the disclosure banner")
	}

	// A selection no tracked bucket can match renders the untracked note
	// (also a banner) instead of bare zeros.
	untrackedFilters := statistics.InteractionFilters{DecisionModel: "never-seen"}
	untrackedSnap := c.InteractionSnapshot(statistics.GlobalScope(), untrackedFilters)
	if untrackedSnap.UntrackedSelection == "" {
		t.Fatalf("an untracked model selection must carry the disclosure note")
	}
	untracked := Report{Snapshot: untrackedSnap, Scope: statistics.GlobalScope(), Filters: untrackedFilters}
	flaggedUntracked := renderReport(t, untracked)
	if bytes.Equal(flaggedUntracked, clean) {
		t.Fatalf("an untracked selection must render the disclosure banner")
	}
}

func TestRenderLongIDsAndUnicode(t *testing.T) {
	c := newSeededCollector(t)

	// An oversized model ID (over the 128-byte cap) folds into overflow in
	// the collector; as a FILTER value it selects nothing tracked — and the
	// display-ellipsized header must still render. A Cyrillic model name
	// must render too (embedded font coverage).
	longID := "very-long-decision-model-" + string(bytes.Repeat([]byte("x"), 120))
	cyrillic := "модель-решения"

	for _, model := range []string{longID, cyrillic} {
		epoch := c.BeginSubmission(300)
		c.RecordInference(statistics.InferenceAttempt{
			Epoch: epoch,
			Keys:  statistics.InteractionKeys{ChatID: 300, Profile: "talk_low", DecisionModel: model, Mode: statistics.ModeLive},
			Model: model, Latency: 150 * time.Millisecond, ErrClass: statistics.InferenceSuccess,
		})
	}

	for _, model := range []string{longID, cyrillic} {
		scope, filters := statistics.GlobalScope(), statistics.InteractionFilters{DecisionModel: model}
		input := Report{
			Snapshot: c.InteractionSnapshot(scope, filters),
			Scope:    scope,
			Filters:  filters,
			Annotations: &ThresholdAnnotations{
				ProfileID:         "talk_low",
				ReactionThreshold: 0.9,
				ReplyThreshold:    0.7,
				MaxIntrusion:      0.8,
			},
		}
		decodeReport(t, input)
	}

	// The oversized ID must genuinely be folded, not tracked verbatim.
	snapshot := c.InteractionSnapshot(statistics.GlobalScope(), statistics.InteractionFilters{})
	if snapshot.OversizedIDFolds == 0 {
		t.Fatalf("the oversized model ID must overflow in the collector")
	}
}

func TestBinLabelsDeriveFromSnapshot(t *testing.T) {
	// Score histograms: one label per bin, lower-edge humanized (20 equal
	// bins on [0,1], 21 edges).
	edges := make([]float64, 21)
	for i := range edges {
		edges[i] = float64(i) / 20
	}
	score := statistics.HistogramSnapshot{Edges: edges, Counts: make([]uint64, 20)}
	labels := binLabels(score)
	if len(labels) != 20 {
		t.Fatalf("score bin labels = %d, want 20", len(labels))
	}
	if labels[0] != "0" || labels[1] != "0.05" {
		t.Fatalf("score label edges wrong: %q, %q", labels[0], labels[1])
	}

	// Latency histograms: humanized lower edges and an open-ended terminal
	// marker.
	latency := statistics.HistogramSnapshot{
		Edges:     []float64{100, 250, 500, 1000, 2500, 5000, 10000, 30000, 60000, 120000},
		OpenEnded: true,
		Counts:    make([]uint64, 10),
	}
	latencyLabels := binLabels(latency)
	if len(latencyLabels) != 10 {
		t.Fatalf("latency bin labels = %d, want 10", len(latencyLabels))
	}
	if latencyLabels[0] != "100" || latencyLabels[3] != "1s" {
		t.Fatalf("latency labels not humanized: %v", latencyLabels)
	}
	if latencyLabels[len(latencyLabels)-1] != "≥120s" {
		t.Fatalf("open-ended terminal bin must be marked: %q", latencyLabels[len(latencyLabels)-1])
	}
}

func TestHumanMsAndTrimFloat(t *testing.T) {
	cases := []struct {
		value float64
		want  string
	}{
		{100, "100"},
		{1000, "1s"},
		{2500, "2.5s"},
		{120000, "120s"},
	}
	for _, tc := range cases {
		if got := humanMs(tc.value); got != tc.want {
			t.Fatalf("humanMs(%v) = %q, want %q", tc.value, got, tc.want)
		}
	}
	if got := trimFloat(7); got != "7" {
		t.Fatalf("trimFloat(7) = %q", got)
	}
	if got := trimFloat(0.75); got != "0.75" {
		t.Fatalf("trimFloat(0.75) = %q", got)
	}
}
