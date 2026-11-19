package statistics

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func newTestCollector(now func() time.Time) *Collector {
	return NewCollector(Config{Now: now})
}

func TestCollectorAccumulatesUsageAndCounters(t *testing.T) {
	t.Parallel()

	stats := newTestCollector(time.Now)

	stats.PrivateRequest()
	stats.Mention()
	stats.AddUsage(10, 20, 30, 0.5)
	stats.LlmTimeout()

	snapshot := stats.GeneralSnapshot()
	if snapshot.PrivateRequests != 1 || snapshot.Mentions != 1 {
		t.Fatalf("unexpected counter snapshot: %#v", snapshot)
	}
	if snapshot.PromptTokens != 10 || snapshot.CompletionTokens != 20 || snapshot.TotalTokens != 30 {
		t.Fatalf("unexpected token snapshot: %#v", snapshot)
	}
	if snapshot.TotalCost != 0.5 || snapshot.LlmTimeouts != 1 {
		t.Fatalf("unexpected totals snapshot: %#v", snapshot)
	}
}

func TestCollectorRecordsAllGeneralCounters(t *testing.T) {
	t.Parallel()

	stats := newTestCollector(time.Now)

	stats.GroupRequest()
	stats.GroupRequest()
	stats.PrivateRequest()
	stats.InlineQuery()
	stats.SummarizeRequest()
	stats.ChatHistoryReset()

	snapshot := stats.GeneralSnapshot()
	if snapshot.GroupRequests != 2 ||
		snapshot.PrivateRequests != 1 ||
		snapshot.InlineQueries != 1 ||
		snapshot.SummarizeRequests != 1 ||
		snapshot.ChatHistoryResets != 1 {
		t.Fatalf("unexpected counter snapshot: %#v", snapshot)
	}
}

func TestCollectorRejectsNegativeUsage(t *testing.T) {
	t.Parallel()

	stats := newTestCollector(time.Now)

	stats.AddUsage(-1, 5, 10, 1)

	snapshot := stats.GeneralSnapshot()
	if snapshot.PromptTokens != 0 || snapshot.CompletionTokens != 0 || snapshot.TotalTokens != 0 || snapshot.TotalCost != 0 {
		t.Fatalf("negative usage must be rejected, got %#v", snapshot)
	}
}

func TestCollectorUptimeSurvivesGeneralReset(t *testing.T) {
	t.Parallel()

	current := time.Unix(1_000_000, 0)
	stats := newTestCollector(func() time.Time { return current })

	stats.PrivateRequest()

	first := stats.GeneralSnapshot()
	if !first.RunningSince.Equal(current) || first.Uptime != "0s" {
		t.Fatalf("unexpected first snapshot clocks: %#v", first)
	}

	current = current.Add(10 * time.Minute)
	stats.ResetGeneral()
	stats.PrivateRequest()

	second := stats.GeneralSnapshot()
	if !second.RunningSince.Equal(first.RunningSince) {
		t.Fatalf("general reset must not restart the uptime baseline: %#v vs %#v", second.RunningSince, first.RunningSince)
	}
	if second.Uptime != "10m0s" {
		t.Fatalf("uptime must keep accruing across resets, got %q", second.Uptime)
	}
	if second.PrivateRequests != 1 {
		t.Fatalf("reset must zero counters and restart observation: %#v", second)
	}
	if !second.PeriodStartedAt.Equal(current) {
		t.Fatalf("observation period must restart at the reset instant, got %#v", second.PeriodStartedAt)
	}
}

func TestGeneralJSONMatchesLegacyShape(t *testing.T) {
	t.Parallel()

	stats := newTestCollector(time.Now)
	stats.GroupRequest()
	stats.PrivateRequest()
	stats.InlineQuery()
	stats.Mention()
	stats.SummarizeRequest()
	stats.ChatHistoryReset()
	stats.AddUsage(10, 20, 30, 0.5)
	stats.LlmTimeout()

	rendered := GeneralJSON(stats.GeneralSnapshot())

	// The legacy representation is MarshalIndent of the flat counter struct:
	// uptime first, counters in declaration order, no non-serialized fields.
	var decoded map[string]any
	if err := json.Unmarshal([]byte(rendered), &decoded); err != nil {
		t.Fatalf("rendered stats must be valid JSON: %v", err)
	}
	if len(decoded) != 12 {
		t.Fatalf("unexpected field count in rendered stats: %d (%v)", len(decoded), decoded)
	}
	wantKeys := []string{
		"uptime", "group_requests", "private_requests", "inline_queries",
		"mentions", "summarize_requests", "chat_history_resets",
		"prompt_tokens", "completion_tokens", "total_tokens", "total_cost",
		"llm_timeouts",
	}
	for _, key := range wantKeys {
		if _, ok := decoded[key]; !ok {
			t.Fatalf("rendered stats missing legacy key %q: %s", key, rendered)
		}
	}
	if !strings.HasPrefix(rendered, "{\n  \"uptime\":") {
		t.Fatalf("rendered stats must keep the legacy field order and indentation: %q", rendered[:40])
	}
	if decoded["total_tokens"].(float64) != 30 || decoded["llm_timeouts"].(float64) != 1 {
		t.Fatalf("unexpected rendered values: %s", rendered)
	}
}
