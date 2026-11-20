package pngreport

import (
	"fmt"
	"strings"

	"github.com/go-analyze/charts"

	"telegram-ollama-reply-bot/internal/statistics"
)

const (
	// maxToolRows bounds the chart and the table: a large MCP registry never
	// produces an unreadable or unbounded image. Zero-use tools only fill
	// rows the used tools leave free; the footer discloses the rest.
	maxToolRows = 30

	toolRowHeight      = 26
	toolTableRowHeight = 20
	toolHeaderHeight   = 132
	toolPanelPadding   = 56
	toolFooterHeight   = 56

	toolLabelLimit = 40
)

var (
	colorToolSuccess = charts.Color{R: 0, G: 114, B: 178, A: 255} // blue
	colorToolFailure = charts.Color{R: 213, G: 94, B: 0, A: 255}  // vermillion
)

// ToolReport is the presenter input for the tool statistics report: one
// immutable snapshot (the SAME value the JSON export renders) with the scope
// it was taken under.
type ToolReport struct {
	Snapshot statistics.ToolSnapshot
	Scope    statistics.InteractionScope
	// ScopeLabel names the scope in the header ("Global" or the chat).
	ScopeLabel string
}

// RenderTools produces the tool statistics PNG: a horizontal stacked bar
// chart of successful and unsuccessful invocation attempts per tool (one row
// per tool, descending), followed by a compact numeric table for the same
// rows. The image height follows the row count and is bounded by
// maxToolRows. A chart failure fails the whole render; the caller falls back
// to the JSON export alone.
func RenderTools(input ToolReport) ([]byte, error) {
	snap := input.Snapshot
	rows, omitted, omittedZero := selectToolRows(snap.Tools)
	hasData := len(snap.Tools) > 0 || toolTurnTotal(snap) > 0

	chartHeight := toolPanelPadding + len(rows)*toolRowHeight + 40
	tableHeight := toolPanelPadding + (len(rows)+1)*toolTableRowHeight
	height := canvasTopMargin + toolHeaderHeight + bandPadding
	if hasData && len(rows) > 0 {
		height += chartHeight + bandPadding + tableHeight + bandPadding
	} else {
		height += 200
	}
	height += toolFooterHeight

	painter := charts.NewPainter(charts.PainterOptions{
		Width:        reportWidth,
		Height:       height,
		OutputFormat: charts.ChartOutputPNG,
		Font:         charts.GetDefaultFont(),
	})
	painter.FilledRect(0, 0, reportWidth, height, colorCanvas, colorCanvas, 0)

	top := renderToolHeader(painter, input, canvasTopMargin)
	if !hasData || len(rows) == 0 {
		empty := drawPanel(painter, charts.NewBox(24, top, reportWidth-24, top+180), "No tool statistics in this period")
		empty.Text("No turn has been recorded yet: every counter is zero.", 20, 70, 0, fontStyle(fontSizeBody, colorTextDim))
		renderToolFooter(painter, height, 0, 0)

		return painter.Bytes()
	}

	if err := renderToolChart(painter, charts.NewBox(24, top, reportWidth-24, top+chartHeight), rows); err != nil {
		return nil, err
	}
	top += chartHeight + bandPadding
	renderToolTable(painter, charts.NewBox(24, top, reportWidth-24, top+tableHeight), rows)
	renderToolFooter(painter, height, omitted, omittedZero)

	return painter.Bytes()
}

// selectToolRows takes the first maxToolRows rows of the snapshot order
// (attempts descending, then advertised turns, then name), so zero-use tools
// appear only after every used tool.
func selectToolRows(all []statistics.ToolRowView) (rows []statistics.ToolRowView, omitted, omittedZero int) {
	if len(all) <= maxToolRows {
		return all, 0, 0
	}
	for _, row := range all[maxToolRows:] {
		omitted++
		if row.Counters.Attempts == 0 {
			omittedZero++
		}
	}

	return all[:maxToolRows], omitted, omittedZero
}

func toolTurnTotal(snap statistics.ToolSnapshot) uint64 {
	var total uint64
	for _, v := range snap.Turns {
		total += v.Turns
	}

	return total
}

func toolRowLabel(row statistics.ToolRowView) string {
	switch {
	case row.Tool != nil:
		return ellipsize(*row.Tool, toolLabelLimit)
	case row.Overflow == "unregistered":
		return "(unregistered names)"
	default:
		return "(other registered tools)"
	}
}

func renderToolHeader(p *charts.Painter, input ToolReport, top int) int {
	snap := input.Snapshot
	scope := input.ScopeLabel
	if scope == "" {
		scope = "Global"
	}

	var explicit, proactive statistics.ToolTurnView
	for _, v := range snap.Turns {
		if v.Mode == "proactive" {
			proactive = v
		} else {
			explicit = v
		}
	}
	totals := snap.Totals.Counters
	lines := []string{
		"Tool usage statistics",
		fmt.Sprintf("Scope: %s · period since %s · resets: %d · persistence: %s", scope,
			snap.Period.StartedAt.Format("2006-01-02 15:04"), snap.Period.Resets, snap.Persistence.State),
		fmt.Sprintf("Turns: %s explicit, %s proactive · attempts %s (%s ok, %s failed, %s rejected)",
			formatUint(explicit.Turns), formatUint(proactive.Turns), formatUint(totals.Attempts),
			formatUint(totals.Succeeded), formatUint(totals.Failed), formatUint(totals.Rejected)),
		fmt.Sprintf("Catalog: %d registered, %d tracked, %d omitted", snap.Catalog.Size, snap.Catalog.Tracked, snap.Catalog.Omitted),
	}
	var notes []string
	if !snap.PerChatComplete {
		notes = append(notes, "per-chat attribution incomplete (chat overflow)")
	}
	if !snap.ModelAttributionComplete {
		notes = append(notes, "model attribution incomplete (model overflow)")
	}
	if snap.UntrackedSelection != "" {
		notes = append(notes, snap.UntrackedSelection)
	}
	if len(notes) > 0 {
		lines = append(lines, "⚠ "+strings.Join(notes, "; "))
	}

	band := charts.NewBox(0, top, reportWidth, top+toolHeaderHeight)
	child := p.Child(charts.PainterBoxOption(band))
	for i, line := range lines {
		style := fontStyle(fontSizeBody, colorText)
		if i == 0 {
			style = fontStyle(fontSizeHeader+2, colorText)
		}
		if strings.HasPrefix(line, "⚠") {
			style = fontStyle(fontSizeBody, colorMark)
		}
		child.TextFit(line, 24, 22+i*22, reportWidth-48, style)
	}

	return band.Bottom + bandPadding
}

func renderToolChart(p *charts.Painter, box charts.Box, rows []statistics.ToolRowView) error {
	// Horizontal bar charts lay the first category out at the BOTTOM; the
	// rows arrive in descending order, so they are fed in reverse to put the
	// busiest tool on top.
	labels := make([]string, 0, len(rows))
	success := make([]float64, 0, len(rows))
	failure := make([]float64, 0, len(rows))
	var maxAttempts uint64
	for i := len(rows) - 1; i >= 0; i-- {
		row := rows[i]
		labels = append(labels, fmt.Sprintf("%s · %s", toolRowLabel(row), formatUint(row.Counters.Attempts)))
		success = append(success, float64(row.Counters.Succeeded))
		failure = append(failure, float64(row.Counters.Failed+row.Counters.Rejected))
		if row.Counters.Attempts > maxAttempts {
			maxAttempts = row.Counters.Attempts
		}
	}
	unit := niceUnit(maxAttempts)

	child := drawPanel(p, box, "Invocation attempts per tool (successful vs unsuccessful)")
	// Child boxes are in canvas coordinates (like the interaction report).
	plot := child.Child(charts.PainterBoxOption(charts.NewBox(box.Left+8, box.Top+34, box.Right-8, box.Bottom-8)))

	segmentLabel := charts.SeriesLabel{
		Show: charts.Ptr(true),
		ValueFormatter: func(v float64) string {
			if v == 0 {
				return ""
			}

			return trimFloat(v)
		},
	}

	return plot.BarChart(charts.BarChartOption{
		Theme:        charts.GetDefaultTheme().WithSeriesColors([]charts.Color{colorToolSuccess, colorToolFailure}),
		Padding:      charts.NewBoxEqual(6),
		Horizontal:   true,
		StackSeries:  charts.Ptr(true),
		CategoryAxis: charts.CategoryAxisOption{Labels: labels},
		ValueAxis: []charts.ValueAxisOption{{
			Min:  charts.Ptr(0.0),
			Max:  charts.Ptr(unit * float64(maxAttempts/uint64(unit)+1)), // headroom for the end label
			Unit: unit,
		}},
		Legend: charts.LegendOption{
			SeriesNames: []string{"Successful", "Unsuccessful (failed + rejected)"},
			Offset:      charts.OffsetRight,
			Align:       charts.AlignRight,
		},
		SeriesList: charts.NewSeriesListBar([][]float64{success, failure}, charts.BarSeriesOption{Label: segmentLabel}),
	})
}

// toolTableColumns are the numeric table's column headers and x offsets.
var toolTableColumns = []struct {
	title string
	x     int
}{
	{"Tool", 16}, {"Attempts", 380}, {"Success", 470}, {"Advertised", 560},
	{"Utilization", 660}, {"Recovery", 770}, {"Latency p50 / p90 (ms)", 870},
}

func renderToolTable(p *charts.Painter, box charts.Box, rows []statistics.ToolRowView) {
	child := drawPanel(p, box, "Advertisement, utilization, recovery and latency")
	head := fontStyle(fontSizeBody, colorText)
	body := fontStyle(fontSizeBody, colorTextDim)
	y := 38
	for _, col := range toolTableColumns {
		child.Text(col.title, col.x, y, 0, head)
	}
	for _, row := range rows {
		y += toolTableRowHeight
		cells := []string{
			toolRowLabel(row),
			formatUint(row.Counters.Attempts),
			percent(row.Rates.SuccessRate),
			formatUint(row.Counters.AdvertisedTurns),
			percent(row.Rates.UtilizationRate),
			percent(row.Rates.RecoveryRate),
			latencyPair(row.Percentiles),
		}
		for i, cell := range cells {
			child.Text(cell, toolTableColumns[i].x, y, 0, body)
		}
	}
}

func renderToolFooter(p *charts.Painter, height, omitted, omittedZero int) {
	footer := p.Child(charts.PainterBoxOption(charts.NewBox(24, height-toolFooterHeight, reportWidth-24, height)))
	text := "Approximate rendering — the JSON export is the canonical, self-describing format."
	if omitted > 0 {
		text = fmt.Sprintf("+%d more tools (%d with zero attempts) are in the JSON export. ", omitted, omittedZero) + text
	}
	footer.TextFit(text, 0, 14, reportWidth-48, fontStyle(fontSizeBody, colorTextDim))
}

func percent(rate *float64) string {
	if rate == nil {
		return "–"
	}

	return fmt.Sprintf("%.0f%%", *rate*100)
}

func latencyPair(p statistics.LatencyPercentiles) string {
	value := func(v *statistics.PercentileValue) string {
		switch {
		case v == nil:
			return "–"
		case v.ApproxMs != nil:
			return fmt.Sprintf("%.0f", *v.ApproxMs)
		case v.AtLeastMs != nil:
			return fmt.Sprintf("≥%.0f", *v.AtLeastMs)
		}

		return "–"
	}

	return value(p.P50) + " / " + value(p.P90)
}

// niceUnit picks a 1/2/5 × 10^n tick unit that gives about six integer ticks
// for the largest bar.
func niceUnit(maxValue uint64) float64 {
	if maxValue <= 6 {
		return 1
	}
	raw := float64(maxValue) / 6
	magnitude := 1.0
	for magnitude*10 <= raw {
		magnitude *= 10
	}
	for _, step := range []float64{1, 2, 5, 10} {
		if step*magnitude >= raw {
			return step * magnitude
		}
	}

	return 10 * magnitude
}
