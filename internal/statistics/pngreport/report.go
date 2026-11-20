// Package pngreport renders the interaction-statistics PNG report. It is a
// presenter like the JSON export: it consumes one immutable
// statistics.InteractionSnapshot — the SAME value the JSON renders — plus
// resolved threshold annotations, and never collector state. Telegram
// compresses bot-sent photos, so the report renders compression-aware:
// high-contrast palette, generous font sizes, and plain solid fills.
package pngreport

import (
	"fmt"
	"strings"

	"github.com/go-analyze/charts"

	"telegram-ollama-reply-bot/internal/statistics"
)

// Canvas geometry. The layout stacks fixed-height bands on one canvas; a
// photo-shaped portrait canvas reads well in the Telegram chat bubble.
const (
	reportWidth  = 1200
	reportHeight = 2100

	bandPadding = 12
	// Top margin between the canvas edge and the first band's content.
	canvasTopMargin = 14

	fontSizeHeader = 16.0
	fontSizeBody   = 13.0
)

// ThresholdAnnotations carries the decision-profile thresholds the report
// marks on the score charts. The admin UI resolves them from the CURRENT
// preset values (interactivity.ProfileByID) for a single-profile view;
// historical presets are not reconstructible and are not pretended to be.
type ThresholdAnnotations struct {
	ProfileID         string
	ReactionThreshold float64
	ReplyThreshold    float64
	MaxIntrusion      float64
}

// Report is the presenter input: one immutable snapshot (the SAME value the
// JSON export renders) with the scope and filters it was taken under, and
// optional threshold annotations for a single-profile view.
type Report struct {
	Snapshot    statistics.InteractionSnapshot
	Scope       statistics.InteractionScope
	Filters     statistics.InteractionFilters
	Annotations *ThresholdAnnotations
}

// Render produces the PNG report. Annotations are marked on the score
// charts when set (a single-profile view); nil renders the multi-profile
// variant without markers. A chart-rendering failure fails the whole
// render: the caller falls back to delivering the JSON export alone.
func Render(input Report) ([]byte, error) {
	snapshot := input.Snapshot
	painter := charts.NewPainter(charts.PainterOptions{
		Width:        reportWidth,
		Height:       reportHeight,
		OutputFormat: charts.ChartOutputPNG,
		Font:         charts.GetDefaultFont(),
	})
	// The painter canvas starts transparent (near-black in PNG viewers):
	// paint the report's white base before any panel.
	painter.FilledRect(0, 0, reportWidth, reportHeight, colorCanvas, colorCanvas, 0)

	if !reportHasData(snapshot) {
		// An empty state under overflow folding may be a FOLDED selection,
		// not a quiet dataset — the banner must render here too, and the
		// text must not claim the dataset is empty when the selection may
		// just be untracked.
		top := renderBanner(painter, snapshot, canvasTopMargin)
		renderHeader(painter, input, top)
		text := "Every counter is zero: no eligible message reached the decision engine yet."
		if snapshot.UntrackedSelection != "" || !snapshot.FilteredTotalsExact {
			text = "No tracked observations match this selection — activity may be folded into overflow (see the banner)."
		}
		empty := drawPanel(painter, charts.NewBox(60, 360, reportWidth-60, 660), "No interaction observations in this period")
		empty.Text(text, 20, 90, 0, fontStyle(fontSizeBody, colorTextDim))

		return painter.Bytes()
	}

	top := renderBanner(painter, snapshot, canvasTopMargin)
	top = renderHeader(painter, input, top)
	top, err := renderSelectedExecuted(painter, snapshot, top)
	if err != nil {
		return nil, err
	}
	top, err = renderBaseScore(painter, snapshot, input.Annotations, top)
	if err != nil {
		return nil, err
	}
	top, err = renderComponentScores(painter, snapshot, top)
	if err != nil {
		return nil, err
	}
	top, err = renderLatency(painter, snapshot, top)
	if err != nil {
		return nil, err
	}
	if err := renderExecution(painter, snapshot, top); err != nil {
		return nil, err
	}

	footer := painter.Child(charts.PainterBoxOption(charts.NewBox(60, reportHeight-40, reportWidth-60, reportHeight)))
	footer.Text("Approximate rendering — the JSON export is the canonical, self-describing format.",
		0, 12, 0, fontStyle(fontSizeBody, colorTextDim))

	return painter.Bytes()
}

// reportHasData reports whether the snapshot carries any observation at all
// (the same emptiness notion as the admin screen's view).
func reportHasData(snapshot statistics.InteractionSnapshot) bool {
	if snapshot.Submission != nil && snapshot.Submission.Submitted > 0 {
		return true
	}
	if snapshot.Preflight != nil && snapshot.Preflight.Totals.Sum() > 0 {
		return true
	}
	if snapshot.Decision.Totals.Outcomes.Sum() > 0 ||
		snapshot.Decision.Totals.EvaluatedAttempts > 0 {
		return true
	}
	if dispatched := snapshot.Execution.Totals.DispatchedReact + snapshot.Execution.Totals.DispatchedReply; dispatched > 0 {
		return true
	}

	return false
}

// --- palette and text ---

var (
	colorCanvas   = charts.Color{R: 255, G: 255, B: 255, A: 255}
	colorText     = charts.Color{R: 40, G: 40, B: 40, A: 255}
	colorTextDim  = charts.Color{R: 110, G: 110, B: 110, A: 255}
	colorBannerBg = charts.Color{R: 198, G: 40, B: 40, A: 255}
	colorBannerFg = charts.Color{R: 255, G: 255, B: 255, A: 255}
	colorPanelBg  = charts.Color{R: 246, G: 247, B: 249, A: 255}
	colorMark     = charts.Color{R: 198, G: 40, B: 40, A: 255}
)

func fontStyle(size float64, color charts.Color) charts.FontStyle {
	return charts.FontStyle{
		Font:      charts.GetDefaultFont(),
		FontColor: color,
		FontSize:  size,
	}
}

// drawPanelTitle writes a small section heading at the panel top.
func drawPanelTitle(p *charts.Painter, text string) {
	p.Text(text, 8, 8, 0, fontStyle(fontSizeHeader, colorText))
}

// drawPanel draws a light background card with the section title and
// returns the child painter for the card.
func drawPanel(p *charts.Painter, box charts.Box, title string) *charts.Painter {
	child := p.Child(charts.PainterBoxOption(box))
	child.FilledRect(0, 0, child.Width(), child.Height(), colorPanelBg, colorPanelBg, 0.0)
	if title != "" {
		drawPanelTitle(child, title)
	}

	return child
}

// --- banner and header ---

// renderBanner draws the disclosure banner when the narrowed view may be
// incomplete; returns the new top offset.
func renderBanner(p *charts.Painter, snapshot statistics.InteractionSnapshot, top int) int {
	var reasons []string
	if !snapshot.FilteredTotalsExact {
		reasons = append(reasons, "filtered totals may be incomplete: overflow folding is active")
	}
	if snapshot.UntrackedSelection != "" {
		reasons = append(reasons, snapshot.UntrackedSelection)
	}
	if len(reasons) == 0 {
		return top
	}

	band := charts.NewBox(0, top, reportWidth, top+64)
	child := p.Child(charts.PainterBoxOption(band))
	child.FilledRect(0, 0, child.Width(), child.Height(), colorBannerBg, colorBannerBg, 0.0)
	child.TextFit("⚠ "+strings.Join(reasons, "; "), 24, 24, reportWidth-48,
		fontStyle(fontSizeHeader, colorBannerFg))

	return band.Bottom + bandPadding
}

// renderHeader draws the report header block; returns the new top offset.
func renderHeader(p *charts.Painter, input Report, top int) int {
	snapshot := input.Snapshot
	lines := []string{
		fmt.Sprintf("Interaction statistics — mode %s, rubric v%d", snapshot.Mode, snapshot.RubricVersion),
		fmt.Sprintf("Observation period since %s (resets: %d)",
			snapshot.Period.StartedAt.Format("2006-01-02 15:04"), snapshot.Period.Resets),
		scopeLine(input),
		"Generated " + snapshot.GeneratedAt.Format("2006-01-02 15:04 MST"),
		fmt.Sprintf("Eligible %s · evaluated %d · selected %d · executed %d",
			eligibleLabel(snapshot), snapshot.Decision.Totals.EvaluatedAttempts,
			snapshot.Decision.Totals.Outcomes.SelectedReact+snapshot.Decision.Totals.Outcomes.SelectedReply,
			snapshot.Execution.Totals.DispatchedReact+snapshot.Execution.Totals.DispatchedReply),
	}

	height := 36*len(lines) + 24
	box := charts.NewBox(24, top, reportWidth-24, top+height)
	child := p.Child(charts.PainterBoxOption(box))
	for i, line := range lines {
		style := fontStyle(fontSizeHeader, colorText)
		if i > 0 {
			style = fontStyle(fontSizeBody, colorTextDim)
		}
		child.TextFit(line, 8, 12+i*36, box.Right-box.Left-16, style)
	}

	return box.Bottom + bandPadding
}

// scopeLine renders the scope and filters of the report's selection. Filter
// values are display-ellipsized — identity keys are never truncated anywhere
// else (oversized IDs fold into overflow; the JSON export is canonical).
func scopeLine(input Report) string {
	scope := "Global"
	if input.Scope.ChatSpecified {
		scope = fmt.Sprintf("chat %d", input.Scope.ChatID)
	}
	line := "Scope: " + scope + " · profile: " + filterValue(input.Filters.Profile) +
		" · model: " + filterValue(input.Filters.DecisionModel)

	return line
}

// filterValue renders a filter value (empty = unfiltered).
func filterValue(value string) string {
	if value == "" {
		return "All"
	}

	return ellipsize(value, 48)
}

// ellipsize cuts a display string with a trailing ellipsis.
func ellipsize(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}

	return string(runes[:limit-1]) + "…"
}

// eligibleLabel renders the eligibility denominator ("n/a" when the
// chat-keyed section is omitted under filters).
func eligibleLabel(snapshot statistics.InteractionSnapshot) string {
	if snapshot.Submission == nil {
		return "n/a"
	}

	return formatUint(snapshot.Submission.Accepted())
}

// --- charts ---

// labelValue is one named count of the paired summary bars.
type labelValue struct {
	label string
	count uint64
}

// histogramChart renders one histogram snapshot as a bar chart in a new
// panel card at the given box. The title carries the sample count.
func histogramChart(p *charts.Painter, box charts.Box, title string, histogram statistics.HistogramSnapshot) error {
	child := drawPanel(p, box, fmt.Sprintf("%s (N=%d)", title, histogramTotal(histogram)))
	// PainterBoxOption boxes are canvas-absolute (they REPLACE the parent
	// box), so the plot area of a nested painter must be computed in root
	// coordinates, not panel-relative ones.
	plot := child.Child(charts.PainterBoxOption(charts.NewBox(
		box.Left+8, box.Top+36, box.Right-8, box.Bottom-8)))

	return plot.BarChart(charts.BarChartOption{
		Padding:      charts.NewBoxEqual(6),
		CategoryAxis: charts.CategoryAxisOption{Labels: binLabels(histogram)},
		// Counts are integers: suggest unit steps so the axis does not
		// invent fractional ticks (0.4 of a reaction reads absurd).
		ValueAxis:  []charts.ValueAxisOption{{Unit: 1}},
		SeriesList: charts.NewSeriesListBar([][]float64{countsToValues(histogram)}),
	})
}

// renderSelectedExecuted draws the selected-action distribution beside the
// executed-effects summary; returns the new top offset.
func renderSelectedExecuted(p *charts.Painter, snapshot statistics.InteractionSnapshot, top int) (int, error) {
	box := charts.NewBox(24, top, reportWidth-24, top+300)
	left := box.Right/2 - 12
	right := box.Right/2 + 12

	decision := snapshot.Decision.Totals.Outcomes
	if err := horizontalCountChart(
		p, charts.NewBox(box.Left, box.Top, left, box.Bottom), "Selected actions",
		[]labelValue{
			{"Ignore/veto", decision.SelectedIgnoreVeto + decision.SelectedIgnoreBelow},
			{"React", decision.SelectedReact},
			{"Reply", decision.SelectedReply},
		}); err != nil {
		return box.Bottom, err
	}

	execution := snapshot.Execution.Totals
	if err := horizontalCountChart(
		p, charts.NewBox(right, box.Top, box.Right, box.Bottom), "Executed effects",
		[]labelValue{
			{"Reactions", execution.ReactionsConfirmed},
			{"Texts", execution.TextsConfirmed},
			{"Other effects", execution.OtherSideEffects},
			{"Failed turns", execution.TurnsFailed},
		}); err != nil {
		return box.Bottom, err
	}

	return box.Bottom + bandPadding, nil
}

// horizontalCountChart renders named counts as a horizontal bar chart with
// explicit value labels, in a new panel card at the given box. The box is
// canvas-absolute (PainterBoxOption boxes REPLACE the parent box).
func horizontalCountChart(p *charts.Painter, box charts.Box, title string, entries []labelValue) error {
	values := make([]float64, 0, len(entries))
	labels := make([]string, 0, len(entries))
	for _, entry := range entries {
		labels = append(labels, entry.label)
		values = append(values, float64(entry.count))
	}

	child := drawPanel(p, box, title)
	plot := child.Child(charts.PainterBoxOption(charts.NewBox(
		box.Left+8, box.Top+36, box.Right-8, box.Bottom-8)))

	return plot.BarChart(charts.BarChartOption{
		Padding:      charts.NewBoxEqual(6),
		Horizontal:   true,
		CategoryAxis: charts.CategoryAxisOption{Labels: labels},
		ValueAxis:    []charts.ValueAxisOption{{Unit: 1}},
		SeriesList: charts.NewSeriesListBar([][]float64{values}, charts.BarSeriesOption{
			Label: charts.SeriesLabel{
				Show:           charts.Ptr(true),
				ValueFormatter: trimFloat,
			},
		}),
	})
}

// renderBaseScore draws the base-score histogram with threshold markers for
// a single-profile view; returns the new top offset. The ruler is its own
// band BELOW the chart — overlaying it would hide the histogram's axis.
func renderBaseScore(p *charts.Painter, snapshot statistics.InteractionSnapshot, annotations *ThresholdAnnotations, top int) (int, error) {
	box := charts.NewBox(24, top, reportWidth-24, top+300)

	title := "Base score"
	if annotations != nil {
		title += " — profile " + annotations.ProfileID
	}
	if err := histogramChart(p, box, title, snapshot.Decision.Totals.ScoreHistograms.Base); err != nil {
		return box.Bottom, err
	}

	bottom := box.Bottom
	if annotations != nil {
		ruler := charts.NewBox(box.Left, bottom+bandPadding, box.Right, bottom+bandPadding+92)
		drawThresholdRuler(p, ruler, *annotations)
		bottom = ruler.Bottom
	}

	return bottom + bandPadding, nil
}

// drawThresholdRuler draws a [0,1] scale with the profile's threshold
// markers as dashed vertical lines — the exact position mapping is ours,
// decoupled from the bar chart's internal geometry.
func drawThresholdRuler(p *charts.Painter, box charts.Box, annotations ThresholdAnnotations) {
	child := p.Child(charts.PainterBoxOption(box))
	child.FilledRect(0, 0, child.Width(), child.Height(), colorPanelBg, colorPanelBg, 0.0)
	drawPanelTitle(child, "Decision thresholds (current preset)")

	scaleLeft, scaleRight := 30, child.Width()-30
	axisY := child.Height() - 30

	// The scale axis and the markers are plain filled rectangles: exact
	// geometry, high contrast, no dependency on chart internals.
	child.FilledRect(scaleLeft, axisY, scaleRight, axisY+2, colorTextDim, colorTextDim, 0)

	markers := []struct {
		value float64
		label string
	}{
		{annotations.ReactionThreshold, "react"},
		{annotations.ReplyThreshold, "reply"},
		{annotations.MaxIntrusion, "veto"},
	}
	for _, marker := range markers {
		value := marker.value
		if value < 0 {
			value = 0
		}
		if value > 1 {
			value = 1
		}
		x := scaleLeft + int(value*float64(scaleRight-scaleLeft))
		child.FilledRect(x-1, axisY-24, x+1, axisY+2, colorMark, colorMark, 0)
		child.Text(fmt.Sprintf("%s %.2f", marker.label, marker.value), x-24, axisY-42, 0,
			fontStyle(fontSizeBody, colorMark))
	}

	child.Text("0", scaleLeft-10, axisY+6, 0, fontStyle(fontSizeBody, colorTextDim))
	child.Text("1", scaleRight-6, axisY+6, 0, fontStyle(fontSizeBody, colorTextDim))
}

// renderComponentScores draws the five component score histograms; returns
// the new top offset.
func renderComponentScores(p *charts.Painter, snapshot statistics.InteractionSnapshot, top int) (int, error) {
	histograms := snapshot.Decision.Totals.ScoreHistograms
	type panel struct {
		title     string
		histogram statistics.HistogramSnapshot
	}
	panels := []panel{
		{"Relevance", histograms.Relevance},
		{"Contribution", histograms.Contribution},
		{"Intrusion", histograms.Intrusion},
		{"React suitability", histograms.ReactSuitability},
		{"Reply suitability", histograms.ReplySuitability},
	}

	rowHeight := 240
	width := reportWidth - 48

	draw := func(items []int, rowTop int) (int, error) {
		cols := len(items)
		gap := 12
		colWidth := (width - gap*(cols-1)) / cols
		for i, index := range items {
			left := 24 + i*(colWidth+gap)
			if err := histogramChart(p, charts.NewBox(left, rowTop, left+colWidth, rowTop+rowHeight),
				panels[index].title, panels[index].histogram); err != nil {
				return rowTop + rowHeight, err
			}
		}

		return rowTop + rowHeight + bandPadding, nil
	}

	top, err := draw([]int{0, 1, 2}, top)
	if err != nil {
		return top, err
	}
	top, err = draw([]int{3, 4}, top)
	if err != nil {
		return top, err
	}

	return top, nil
}

// renderLatency draws the decision and inference latency histograms with
// their approximate percentiles; returns the new top offset.
func renderLatency(p *charts.Painter, snapshot statistics.InteractionSnapshot, top int) (int, error) {
	box := charts.NewBox(24, top, reportWidth-24, top+330)
	mid := box.Left + (box.Right-box.Left)/2
	latency := snapshot.Decision.Totals.Latency

	if err := histogramChart(p, charts.NewBox(box.Left, box.Top, mid-6, box.Bottom-40),
		"Decision latency (ms)", latency.Decision); err != nil {
		return box.Bottom, err
	}
	captionLeft := p.Child(charts.PainterBoxOption(charts.NewBox(box.Left, box.Bottom-40, mid-6, box.Bottom)))
	captionLeft.Text(percentileLine(latency.Percentiles.Decision), 16, 6, 0, fontStyle(fontSizeBody, colorTextDim))

	if err := histogramChart(p, charts.NewBox(mid+6, box.Top, box.Right, box.Bottom-40),
		"Inference latency (ms)", latency.Inference); err != nil {
		return box.Bottom, err
	}
	captionRight := p.Child(charts.PainterBoxOption(charts.NewBox(mid+6, box.Bottom-40, box.Right, box.Bottom)))
	captionRight.Text(percentileLine(latency.Percentiles.Inference), 16, 6, 0, fontStyle(fontSizeBody, colorTextDim))

	return box.Bottom + bandPadding, nil
}

// renderExecution draws the execution-duration histogram and the failure
// summary.
func renderExecution(p *charts.Painter, snapshot statistics.InteractionSnapshot, top int) error {
	box := charts.NewBox(24, top, reportWidth-24, reportHeight-64)
	mid := box.Left + (box.Right-box.Left)/2

	if err := histogramChart(p, charts.NewBox(box.Left, box.Top, mid-6, box.Bottom-40),
		"Execution duration (ms)", snapshot.Execution.Totals.Duration); err != nil {
		return err
	}
	captionLeft := p.Child(charts.PainterBoxOption(charts.NewBox(box.Left, box.Bottom-40, mid-6, box.Bottom)))
	captionLeft.Text(percentileLine(snapshot.Execution.Totals.Percentiles), 16, 6, 0, fontStyle(fontSizeBody, colorTextDim))

	failures := snapshot.Execution.Totals
	child := drawPanel(p, charts.NewBox(mid+6, box.Top, box.Right, box.Bottom), "Failures and outcomes")
	rows := []labelValue{
		{"Timeout", failures.Failures.Timeout},
		{"Cancelled", failures.Failures.Cancelled},
		{"Generation", failures.Failures.Generation},
		{"Delivery", failures.Failures.Delivery},
		{"Abandoned target (defensive)", failures.Failures.AbandonedTarget},
		{"Turns without effect", failures.TurnsWithoutEffect},
	}
	for i, row := range rows {
		child.Text(row.label, 20, 44+i*30, 0, fontStyle(fontSizeBody, colorTextDim))
		child.Text(formatUint(row.count), child.Width()-80, 44+i*30, 0, fontStyle(fontSizeBody, colorText))
	}

	return nil
}

// --- helpers ---

func histogramTotal(histogram statistics.HistogramSnapshot) uint64 {
	var total uint64
	for _, count := range histogram.Counts {
		total += count
	}

	return total
}

func countsToValues(histogram statistics.HistogramSnapshot) []float64 {
	values := make([]float64, len(histogram.Counts))
	for i, count := range histogram.Counts {
		values[i] = float64(count)
	}

	return values
}

// binLabels renders one category label per bin, each bin labeled with its
// (humanized) LOWER bound. The two histogram shapes differ: open-ended
// (latency) histograms have counts = len(edges)+1 — bin 0 is
// [0, edges[0]), finite bin i is [edges[i-1], edges[i]), and the terminal
// bin is marked ≥ last edge — while score histograms carry one edge per
// bin boundary (bin i = [edges[i], edges[i+1])).
func binLabels(histogram statistics.HistogramSnapshot) []string {
	labels := make([]string, len(histogram.Counts))
	for i := range histogram.Counts {
		switch {
		case histogram.OpenEnded && i == 0:
			labels[i] = humanMs(0)
		case histogram.OpenEnded && i < len(histogram.Edges):
			labels[i] = humanMs(histogram.Edges[i-1])
		case i < len(histogram.Edges):
			labels[i] = humanMs(histogram.Edges[i])
		}
	}
	if histogram.OpenEnded && len(labels) > 0 && len(histogram.Edges) > 0 {
		labels[len(labels)-1] = "≥" + humanMs(histogram.Edges[len(histogram.Edges)-1])
	}

	return labels
}

// humanMs renders a millisecond value compactly.
func humanMs(ms float64) string {
	if ms >= 1000 {
		return trimFloat(ms/1000) + "s"
	}

	return trimFloat(ms)
}

func percentileLine(percentiles statistics.LatencyPercentiles) string {
	part := func(p *statistics.PercentileValue, name string) string {
		if p == nil {
			return name + " n/a"
		}
		if p.AtLeastMs != nil {
			return fmt.Sprintf("%s ≥%s", name, humanMs(*p.AtLeastMs))
		}
		if p.ApproxMs != nil {
			return fmt.Sprintf("%s ≈%s", name, humanMs(*p.ApproxMs))
		}

		return name + " n/a"
	}

	return part(percentiles.P50, "p50") + "  " + part(percentiles.P90, "p90") + "  " + part(percentiles.P99, "p99")
}

func formatUint(value uint64) string {
	return fmt.Sprintf("%d", value)
}

// trimFloat renders integral floats without a decimal tail.
func trimFloat(value float64) string {
	if value == float64(int64(value)) {
		return fmt.Sprintf("%d", int64(value))
	}

	return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.2f", value), "0"), ".")
}
