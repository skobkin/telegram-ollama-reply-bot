package adminui

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	core "telegram-ollama-reply-bot/internal/adminui"
	"telegram-ollama-reply-bot/internal/statistics"
	"telegram-ollama-reply-bot/internal/statistics/pngreport"
)

// testStatsChat is the group chat the export tests seed observations for.
const testStatsChat = int64(-100200300)

// seedInteractionStats records one full observation into the harness's
// statistics collector so the export renders a populated report.
func seedInteractionStats(t *testing.T, h *harness) {
	t.Helper()

	keys := statistics.InteractionKeys{
		ChatID: testStatsChat, Profile: "talk_low", DecisionModel: "m1", Mode: statistics.ModeLive,
	}
	epoch := h.stats.BeginSubmission(testStatsChat)
	h.stats.RecordInference(statistics.InferenceAttempt{
		Epoch: epoch, Keys: keys, Model: "m1",
		Latency: 700 * time.Millisecond, ErrClass: statistics.InferenceSuccess,
	})
	h.stats.RecordDecision(statistics.DecisionObservation{
		Epoch: epoch, Keys: keys, Outcome: statistics.OutcomeSelectedReply,
		Scores:          &statistics.ScoreSet{Base: 0.42, Relevance: 0.6, Contribution: 0.55, Intrusion: 0.2, ReactSuitability: 0.5, ReplySuitability: 0.75},
		DecisionLatency: 1400 * time.Millisecond,
	})
	h.stats.RecordExecutionDispatch(statistics.ExecutionDispatch{
		Epoch: epoch, Keys: keys, GenerationModel: "gen-a", Action: statistics.ActionReply,
	})
	h.stats.RecordExecution(statistics.ExecutionObservation{
		Epoch: epoch, Keys: keys, GenerationModel: "gen-a", Action: statistics.ActionReply,
		TextConfirmed: true, Duration: 2100 * time.Millisecond,
	})
}

// statsHarness wires the full stack with the real PNG renderer.
func statsHarness(t *testing.T) *harness {
	t.Helper()

	return newHarnessFull(t, []int64{testAdmin}, []core.ControllerOption{
		core.WithStatsReport(pngreport.Render),
	})
}

// exportViaUI drives the admin UI to the statistics screen and taps export.
func exportViaUI(t *testing.T, h *harness) {
	t.Helper()

	seedInteractionStats(t, h)
	h.open(t)
	h.tap(t, core.VerbStats)
	h.tap(t, core.VerbStatsExport)
}

func TestStatsExportDeliversPhotoAndDocument(t *testing.T) {
	h := statsHarness(t)
	exportViaUI(t, h)

	if got := h.api.callCount("sendPhoto"); got != 1 {
		t.Fatalf("export must send one photo, got %d", got)
	}
	if got := h.api.callCount("sendDocument"); got != 1 {
		t.Fatalf("export must send one document, got %d", got)
	}

	photos := h.api.requestsOf("sendPhoto")
	if !strings.HasPrefix(photos[0].Body, "document:interaction-stats-") ||
		!strings.Contains(photos[0].Body, ".png\n") {
		t.Fatalf("photo upload wrong: %q", photos[0].Body[:60])
	}

	documents := h.api.requestsOf("sendDocument")
	body := documents[0].Body
	if idx := strings.IndexByte(body, '\n'); idx >= 0 {
		body = body[idx+1:]
	}
	var decoded struct {
		Schema  string `json:"schema"`
		Version int    `json:"version"`
	}
	if err := json.Unmarshal([]byte(body), &decoded); err != nil {
		t.Fatalf("delivered document must decode: %v", err)
	}
	if decoded.Schema != statistics.InteractionSchemaName {
		t.Fatalf("delivered schema = %q", decoded.Schema)
	}
}

func TestStatsExportPhotoFailureStillDeliversDocument(t *testing.T) {
	h := statsHarness(t)
	h.api.failMethods["sendPhoto"] = errors.New("photo upload failed")
	defer func() { delete(h.api.failMethods, "sendPhoto") }()

	exportViaUI(t, h)

	// The photo attempt happened and failed; the canonical JSON must still
	// arrive — the failure modes are independent.
	if got := h.api.callCount("sendPhoto"); got != 1 {
		t.Fatalf("the photo must be attempted, got %d sends", got)
	}
	if got := h.api.callCount("sendDocument"); got != 1 {
		t.Fatalf("the JSON document must still be delivered, got %d", got)
	}

	// The upload failure is logged sanitized: the typed name only — never
	// the raw transport detail (which could embed the token-bearing URL).
	logs := h.logs.String()
	if !strings.Contains(logs, "admin ui photo send failed") {
		t.Fatalf("the photo failure must be logged: %s", logs)
	}
	if strings.Contains(logs, "photo upload failed") {
		t.Fatalf("the raw failure detail must not be logged: %s", logs)
	}
}
