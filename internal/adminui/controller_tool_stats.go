package adminui

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"telegram-ollama-reply-bot/internal/statistics"
	"telegram-ollama-reply-bot/internal/statistics/pngreport"
)

// ToolReportRenderer renders the tool statistics PNG presenter for one
// snapshot (internal/statistics/pngreport). Like StatsReportRenderer it is an
// injectable seam: tests exercise the JSON-only path and failing renderers
// without a chart library in play.
type ToolReportRenderer func(pngreport.ToolReport) ([]byte, error)

// WithToolStatsReport wires the PNG renderer of the tool statistics export.
func WithToolStatsReport(render ToolReportRenderer) ControllerOption {
	return func(o *ControllerOptions) {
		o.ToolStatsReport = render
	}
}

// toolStatsTopRows bounds the per-tool lines on the admin screen; the full
// list lives in the JSON export and the PNG.
const toolStatsTopRows = 8

// toolStatsData assembles the tool statistics screen from one snapshot under
// the session's scope.
func (c *Controller) toolStatsData(ctx context.Context, state ScreenState) *ToolStatsView {
	scope := statsScopeOf(state)
	snapshot := c.stats.ToolSnapshot(scope)

	view := &ToolStatsView{
		ScopeLabel:      "Global",
		PeriodStartedAt: snapshot.Period.StartedAt,
		PeriodResets:    snapshot.Period.Resets,
		Persistence:     persistenceLine(snapshot.Persistence),
		Narrowed:        state.ChatID != 0,
	}
	if state.ChatID != 0 {
		view.ScopeLabel = fmt.Sprintf("%s (%d)", truncateRunes(c.chatDisplayName(ctx, state.ChatID), 32), state.ChatID)
	}

	var warnings []string
	if snapshot.UntrackedSelection != "" {
		warnings = append(warnings, snapshot.UntrackedSelection)
	}
	if !snapshot.PerChatComplete {
		warnings = append(warnings, "per-chat attribution is incomplete: chat overflow folded some observations")
	}
	if state.ChatID == 0 && !snapshot.ModelAttributionComplete {
		warnings = append(warnings, "model attribution is incomplete: model overflow folded some observations")
	}
	if snapshot.Catalog.Omitted > 0 {
		warnings = append(warnings, fmt.Sprintf("%d registered tools are folded into the other_registered row", snapshot.Catalog.Omitted))
	}
	switch snapshot.Persistence.State {
	case statistics.PersistWriteFailing:
		warnings = append(warnings, "saving statistics is failing; recent data is held in memory and retried")
	case statistics.PersistDisabledNewerFormat, statistics.PersistDisabledQuarantineFailed:
		warnings = append(warnings, "persistence is disabled ("+persistenceReason(snapshot.Persistence.State)+"); statistics are memory-only")
	case statistics.PersistEnabled, statistics.PersistNotConfigured:
	}
	view.Warning = strings.Join(warnings, "; ")

	var turns, withCalls uint64
	var explicit, proactive uint64
	for _, t := range snapshot.Turns {
		turns += t.Turns
		withCalls += t.TurnsWithCalls
		if t.Mode == "proactive" {
			proactive = t.Turns
		} else {
			explicit = t.Turns
		}
	}
	view.HasData = turns > 0 || snapshot.Totals.Counters.Attempts > 0
	if !view.HasData {
		return view
	}

	totals := snapshot.Totals.Counters
	view.Summary = append(view.Summary,
		fmt.Sprintf("Turns %d (explicit %d · proactive %d) · with tool calls %d", turns, explicit, proactive, withCalls),
		fmt.Sprintf("Attempts %d · ok %d (%s) · failed %d · rejected %d",
			totals.Attempts, totals.Succeeded, rateText(snapshot.Totals.Rates.SuccessRate), totals.Failed, totals.Rejected),
	)

	used := 0
	var unusedAdvertised, neverAdvertised int
	for _, row := range snapshot.Tools {
		switch {
		case row.Counters.Attempts == 0 && row.AdvertisedUnused:
			unusedAdvertised++
		case row.Counters.Attempts == 0:
			neverAdvertised++
		}
	}
	view.Summary = append(view.Summary, "", "Top tools (calls · ok · advertised · utilization · recovery):")
	for _, row := range snapshot.Tools {
		if row.Counters.Attempts == 0 || used >= toolStatsTopRows {
			break
		}
		used++
		view.Summary = append(view.Summary, fmt.Sprintf("• %s — %d · %s · %d · %s · %s · %s",
			toolRowName(row), row.Counters.Attempts, rateText(row.Rates.SuccessRate), row.Counters.AdvertisedTurns,
			rateText(row.Rates.UtilizationRate), rateText(row.Rates.RecoveryRate), latencyText(row.Percentiles)))
	}
	if used == 0 {
		view.Summary = append(view.Summary, "• no tool was called yet")
	}
	view.Summary = append(view.Summary, fmt.Sprintf("Zero-use: %d advertised but never called · %d never advertised", unusedAdvertised, neverAdvertised))

	if classes := topErrorClasses(snapshot.Totals.ErrorClasses); classes != "" {
		view.Summary = append(view.Summary, "Errors: "+classes)
	}
	if totals.Retries > 0 {
		view.Summary = append(view.Summary, fmt.Sprintf("Repeats after an error %d · recovered %d (recovery %s)",
			totals.Retries, totals.RecoveredCalls, rateText(snapshot.Totals.Rates.RecoveryRate)))
	}
	if effects := snapshot.Totals.SideEffects; effects.Confirmed+effects.NotApplied+effects.Unknown > 0 {
		view.Summary = append(view.Summary, fmt.Sprintf("Side effects: confirmed %d · not applied %d · unknown %d",
			effects.Confirmed, effects.NotApplied, effects.Unknown))
	}
	if models := modelLines(snapshot.Models); len(models) > 0 {
		view.Summary = append(view.Summary, "", "Models (turns · attempts · unsuccessful):")
		view.Summary = append(view.Summary, models...)
	}

	return view
}

func toolRowName(row statistics.ToolRowView) string {
	switch {
	case row.Tool != nil:
		return truncateRunes(*row.Tool, 36)
	case row.Overflow == "unregistered":
		return "(unregistered names)"
	default:
		return "(other registered tools)"
	}
}

func rateText(rate *float64) string {
	if rate == nil {
		return "n/a"
	}

	return fmt.Sprintf("%.0f%%", *rate*100)
}

func latencyText(p statistics.LatencyPercentiles) string {
	value := func(v *statistics.PercentileValue) string {
		switch {
		case v == nil:
			return "–"
		case v.ApproxMs != nil:
			return msText(*v.ApproxMs)
		case v.AtLeastMs != nil:
			return "≥" + msText(*v.AtLeastMs)
		}

		return "–"
	}

	return "p50 " + value(p.P50) + " / p90 " + value(p.P90)
}

func msText(ms float64) string {
	if ms >= 1000 {
		return fmt.Sprintf("%.1fs", ms/1000)
	}

	return fmt.Sprintf("%.0fms", ms)
}

func topErrorClasses(c statistics.ToolErrorClasses) string {
	type entry struct {
		name  string
		count uint64
	}
	entries := []entry{
		{"invalid_arguments", c.InvalidArguments}, {"not_found", c.NotFound}, {"unavailable", c.Unavailable},
		{"rate_limited", c.RateLimited}, {"permanent", c.Permanent}, {"transient", c.Transient},
		{"cancelled", c.Cancelled}, {"ambiguous_outcome", c.AmbiguousOutcome}, {"other", c.Other},
	}
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].count > entries[j].count })
	var parts []string
	for _, e := range entries {
		if e.count == 0 || len(parts) == 4 {
			break
		}
		parts = append(parts, fmt.Sprintf("%s %d", e.name, e.count))
	}

	return strings.Join(parts, " · ")
}

func modelLines(models []statistics.ToolModelView) []string {
	sort.SliceStable(models, func(i, j int) bool { return models[i].Turns > models[j].Turns })
	lines := make([]string, 0, 4)
	for _, m := range models {
		if len(lines) == 4 {
			break
		}
		name := "(other models)"
		if m.Model != nil {
			name = truncateRunes(*m.Model, 28)
		}
		lines = append(lines, fmt.Sprintf("• %s [%s] — %d · %d · %d", name, m.Mode, m.Turns, m.Attempts, m.Unsuccessful))
	}

	return lines
}

func persistenceLine(info statistics.PersistenceInfo) string {
	line := "Persistence: " + string(info.State)
	if info.Restored {
		line += " · restored at startup"
	}
	if info.LastSavedAt != nil {
		line += " · last saved " + info.LastSavedAt.Format(time.DateTime)
	}

	return line
}

// handleToolStatsExport renders the versioned JSON export and — when a
// renderer is wired — the PNG report from ONE snapshot taken under the
// session's scope, delivered to the admin DM. A PNG failure never blocks the
// JSON (and vice versa delivery of each artifact stays independent in the
// adapter).
func (c *Controller) handleToolStatsExport(ctx context.Context, s *Session, fx *fx) error {
	if s.Screen.Kind != ScreenToolStats || c.stats == nil {
		return errInvalidArg
	}
	scope := statsScopeOf(s.Screen)
	snapshot := c.stats.ToolSnapshot(scope)
	stamp := c.manager.Now().Format("20060102-150405")

	if c.opts.ToolStatsReport != nil {
		label := "Global"
		if s.Screen.ChatID != 0 {
			label = fmt.Sprintf("%s (%d)", truncateRunes(c.chatDisplayName(ctx, s.Screen.ChatID), 32), s.Screen.ChatID)
		}
		png, err := c.opts.ToolStatsReport(pngreport.ToolReport{Snapshot: snapshot, Scope: scope, ScopeLabel: label})
		if err != nil {
			fx.message = &TextMessage{Text: "⚠️ PNG report rendering failed — only the JSON export is attached."}
		} else {
			fx.photo = &PhotoPayload{Filename: "tool-stats-" + stamp + ".png", Content: png}
		}
	}

	payload, err := statistics.ToolJSON(snapshot, scope)
	if err != nil {
		return err
	}
	fx.document = &DocumentPayload{Filename: "tool-stats-" + stamp + ".json", Content: payload}

	return nil
}
