package chart

import (
	"errors"
	"fmt"
	"math"
	"strconv"

	"github.com/go-analyze/charts"
)

// ErrRender reports a failure inside the rendering library; the cause is
// intentionally not wrapped so library text never reaches a model.
var ErrRender = errors.New("chart rendering failed")

var (
	white     = charts.Color{R: 255, G: 255, B: 255, A: 255}
	black     = charts.Color{A: 255}
	axisColor = charts.Color{R: 60, G: 60, B: 60, A: 255}
	// Compression-aware, high-contrast palette (Telegram recompresses photos).
	seriesColors = []charts.Color{
		{R: 31, G: 119, B: 180, A: 255},
		{R: 255, G: 127, B: 14, A: 255},
		{R: 44, G: 160, B: 44, A: 255},
		{R: 214, G: 39, B: 40, A: 255},
		{R: 148, G: 103, B: 189, A: 255},
		{R: 140, G: 86, B: 75, A: 255},
		{R: 227, G: 119, B: 194, A: 255},
		{R: 23, G: 190, B: 207, A: 255},
		{R: 188, G: 189, B: 34, A: 255},
		{R: 127, G: 127, B: 127, A: 255},
		{R: 0, G: 0, B: 0, A: 255},
		{R: 255, G: 187, B: 120, A: 255},
	}
)

const (
	valueLabelPointLimit = 24
	targetTicks          = 6
	labelFontSize        = 14.0
	denseLabelFontSize   = 11.0
	titleBaseSize        = 18.0
	titleMinSize         = 12.0
	titleBand            = 44 // vertical space reserved for the title row
	edgePadding          = 12
	legendSymbolPx       = 70 // symbol + gaps per legend entry
)

// Render validates the spec and renders it to PNG bytes in memory. Long
// titles, labels and legend names are measured against the real font and
// shortened with an ellipsis (display only) so nothing is clipped or
// overlaps.
func Render(spec Spec) ([]byte, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}

	return recoverRender(func() ([]byte, error) { return renderPNG(spec) })
}

// recoverRender converts a panic inside the rendering library into ErrRender.
func recoverRender(render func() ([]byte, error)) (png []byte, err error) {
	defer func() {
		if r := recover(); r != nil {
			png, err = nil, ErrRender
		}
	}()

	return render()
}

func renderPNG(spec Spec) (png []byte, err error) {
	width, height := spec.size()
	painter := charts.NewPainter(charts.PainterOptions{
		Width:        width,
		Height:       height,
		OutputFormat: charts.ChartOutputPNG,
		Font:         charts.GetDefaultFont(),
	})
	painter.FilledRect(0, 0, width, height, white, white, 0)
	f := fitter{p: painter, width: width, height: height}

	switch spec.Type {
	case TypeLine:
		err = painter.LineChart(f.lineOption(spec))
	case TypeBar:
		err = painter.BarChart(f.barOption(spec))
	default:
		err = painter.PieChart(f.pieOption(spec))
	}
	if err != nil {
		return nil, ErrRender
	}
	// Charts paint their own background over the whole canvas, so the title
	// row (reserved by the chart padding) is drawn last.
	f.drawTitle(spec.Title)
	png, err = painter.Bytes()
	if err != nil || len(png) == 0 {
		return nil, ErrRender
	}

	return png, nil
}

// fitter measures text with the painter's real font.
type fitter struct {
	p             *charts.Painter
	width, height int
}

func (f fitter) measure(text string, rotation, size float64) charts.Box {
	return f.p.MeasureText(text, rotation, charts.FontStyle{FontSize: size, FontColor: black})
}

// fit shortens text with an ellipsis until its horizontal (or, when
// vertical, vertical) extent is at most limit pixels.
func (f fitter) fit(text string, rotation, size float64, limit int, vertical bool) string {
	extent := func(s string) int {
		box := f.measure(s, rotation, size)
		if vertical {
			return box.Height()
		}

		return box.Width()
	}
	if limit < 1 || extent(text) <= limit {
		return text
	}
	runes := []rune(text)
	for n := len(runes) - 1; n >= 1; n-- {
		if candidate := string(runes[:n]) + "…"; extent(candidate) <= limit {
			return candidate
		}
	}

	return "…"
}

func (f fitter) fitAll(labels []string, rotation, size float64, limit int, vertical bool) []string {
	result := make([]string, len(labels))
	for i, label := range labels {
		result[i] = f.fit(label, rotation, size, limit, vertical)
	}

	return result
}

// drawTitle draws the title on its own row, shrinking the font and then
// shortening the text so it always fits the canvas.
func (f fitter) drawTitle(title string) {
	if title == "" {
		return
	}
	size := titleBaseSize
	if f.width < 800 {
		size = 16
	}
	limit := f.width - 2*edgePadding
	for size > titleMinSize && f.measure(title, 0, size).Width() > limit {
		size--
	}
	title = f.fit(title, 0, size, limit, false)
	f.p.Text(title, edgePadding, edgePadding+int(size), 0, charts.FontStyle{FontSize: size, FontColor: black})
}

func (f fitter) padding(spec Spec) charts.Box {
	if spec.Title == "" {
		return charts.NewBoxEqual(edgePadding)
	}

	return charts.NewBox(edgePadding, titleBand, edgePadding, edgePadding)
}

func theme(count int) charts.ColorPalette {
	return charts.GetDefaultTheme().WithSeriesColors(seriesColors[:min(count, len(seriesColors))]).
		WithBackgroundColor(white).WithXAxisColor(axisColor).WithYAxisColor(axisColor)
}

// legend fits the series names so the whole legend stays within the canvas.
func (f fitter) legend(names []string) charts.LegendOption {
	perName := (f.width-2*edgePadding)/min(len(names), 4) - legendSymbolPx
	fitted := f.fitAll(names, 0, labelFontSize, max(perName, 40), false)

	return charts.LegendOption{SeriesNames: fitted, Offset: charts.OffsetRight, Align: charts.AlignRight}
}

func seriesNames(spec Spec) []string {
	names := make([]string, len(spec.Series))
	for i, series := range spec.Series {
		names[i] = series.Name
	}

	return names
}

func values(spec Spec, reverse bool) [][]float64 {
	result := make([][]float64, len(spec.Series))
	for i, series := range spec.Series {
		row := append([]float64(nil), series.Values...)
		if reverse {
			reverseSlice(row)
		}
		result[i] = row
	}

	return result
}

func reverseSlice[T any](s []T) {
	for i, j := 0, len(s)-1; i < j; i, j = i+1, j-1 {
		s[i], s[j] = s[j], s[i]
	}
}

// formatValue renders axis ticks and data labels. Unlike the library default
// it keeps small magnitudes readable (0.0025 stays 0.0025, not 0).
func formatValue(v float64) string {
	if v == 0 {
		return "0"
	}
	if math.Abs(v) >= 1000 {
		return charts.FormatValueHumanizeShort(v, 2, false)
	}

	return strconv.FormatFloat(v, 'g', 4, 64)
}

// niceStep rounds a raw step up to 1, 2 or 5 times a power of ten.
func niceStep(raw float64) float64 {
	if raw <= 0 || math.IsNaN(raw) || math.IsInf(raw, 0) {
		return 1
	}
	exp := math.Pow(10, math.Floor(math.Log10(raw)))
	switch f := raw / exp; {
	case f <= 1:
		return exp
	case f <= 2:
		return 2 * exp
	case f <= 5:
		return 5 * exp
	default:
		return 10 * exp
	}
}

// dataExtent returns the min and max the value axis must cover.
func dataExtent(spec Spec) (lo, hi float64) {
	lo, hi = math.Inf(1), math.Inf(-1)
	if spec.Stacked {
		for i := range spec.Categories {
			var sum float64
			for _, series := range spec.Series {
				sum += series.Values[i]
			}
			lo, hi = math.Min(lo, 0), math.Max(hi, sum)
		}
	} else {
		for _, series := range spec.Series {
			for _, v := range series.Values {
				lo, hi = math.Min(lo, v), math.Max(hi, v)
			}
		}
	}
	if spec.Type == TypeBar {
		lo, hi = math.Min(lo, 0), math.Max(hi, 0)
	}

	return lo, hi
}

// valueAxisRange picks a "nice" axis: round bounds on multiples of the step
// (so zero is a tick whenever the range spans it), headroom beyond the data
// extremes for value labels, and an explicit range for degenerate data.
func valueAxisRange(spec Spec) (lo, hi, step float64) {
	minV, maxV := dataExtent(spec)
	switch {
	case minV == 0 && maxV == 0:
		return 0, 1, 0.2
	case minV == maxV:
		half := math.Abs(minV) / 2
		minV, maxV = minV-half, maxV+half
	}
	step = niceStep((maxV - minV) / targetTicks)
	lo = math.Floor(minV/step) * step
	hi = math.Ceil(maxV/step) * step
	// Keep value labels clear of the axis bounds.
	if minV < 0 && minV-lo < step*0.35 {
		lo -= step
	}
	if maxV > 0 && hi-maxV < step*0.35 {
		hi += step
	}

	return lo, hi, step
}

// valueAxis builds the numeric axis. The title is rotated along the axis in
// vertical charts, so it is fitted to the canvas height (width when the
// value axis is horizontal).
func (f fitter) valueAxis(spec Spec, horizontalBars bool) []charts.ValueAxisOption {
	lo, hi, step := valueAxisRange(spec)
	title := f.fit(spec.YLabel, 0, labelFontSize, int(float64(f.width)*0.7), false)
	if !horizontalBars {
		title = f.fit(spec.YLabel, math.Pi/2, labelFontSize, int(float64(f.height)*0.6), true)
	}

	return []charts.ValueAxisOption{{
		Title:          title,
		Min:            charts.Ptr(lo),
		Max:            charts.Ptr(hi),
		Unit:           step,
		ValueFormatter: formatValue,
	}}
}

// categoryAxis lays category labels out without overlap: horizontal when
// they fit their slot, otherwise rotated, and always shortened to a bounded
// extent. Horizontal bars keep their labels on the left, bounded by width.
func (f fitter) categoryAxis(spec Spec, labels []string) charts.CategoryAxisOption {
	axis := charts.CategoryAxisOption{}
	if spec.XLabel != "" {
		axis.Title = f.fit(spec.XLabel, 0, labelFontSize, int(float64(f.width)*0.8), false)
	}
	if spec.Orientation == OrientationHorizontal {
		axis.Labels = f.fitAll(labels, 0, labelFontSize, f.width/4, false)
		if spec.XLabel != "" {
			axis.Title = f.fit(spec.XLabel, math.Pi/2, labelFontSize, int(float64(f.height)*0.6), true)
		}

		return axis
	}

	size := labelFontSize
	if len(labels) > 20 {
		size = denseLabelFontSize
	}
	slot := (f.width - 120) / len(labels)
	widest := 0
	for _, label := range labels {
		widest = max(widest, f.measure(label, 0, size).Width())
	}
	switch {
	case len(labels) > 20:
		axis.LabelRotation = charts.DegreesToRadians(90)
	case widest > slot-8:
		axis.LabelRotation = charts.DegreesToRadians(45)
	}
	if axis.LabelRotation != 0 {
		axis.LabelCount = len(labels)
		axis.Labels = f.fitAll(labels, axis.LabelRotation, size, f.height/4, true)
	} else {
		axis.Labels = f.fitAll(labels, 0, size, slot-8, false)
	}
	axis.LabelFontStyle = charts.FontStyle{FontSize: size, FontColor: axisColor}

	return axis
}

func pointLabel(spec Spec) charts.SeriesLabel {
	if spec.Stacked || len(spec.Series)*len(spec.Categories) > valueLabelPointLimit {
		return charts.SeriesLabel{}
	}

	return charts.SeriesLabel{Show: charts.Ptr(true), ValueFormatter: formatValue}
}

func (f fitter) lineOption(spec Spec) charts.LineChartOption {
	opt := charts.LineChartOption{
		Theme:          theme(len(spec.Series)),
		Padding:        f.padding(spec),
		XAxis:          f.categoryAxis(spec, spec.Categories),
		YAxis:          f.valueAxis(spec, false),
		SeriesList:     charts.NewSeriesListLine(values(spec, false)),
		ValueFormatter: formatValue,
	}
	if len(spec.Series) > 1 {
		opt.Legend = f.legend(seriesNames(spec))
	}

	return opt
}

func (f fitter) barOption(spec Spec) charts.BarChartOption {
	horizontal := spec.Orientation == OrientationHorizontal
	labels := append([]string(nil), spec.Categories...)
	if horizontal {
		// Horizontal bars lay the first category out at the bottom.
		reverseSlice(labels)
	}
	opt := charts.BarChartOption{
		Theme:          theme(len(spec.Series)),
		Padding:        f.padding(spec),
		Horizontal:     horizontal,
		CategoryAxis:   f.categoryAxis(spec, labels),
		ValueAxis:      f.valueAxis(spec, horizontal),
		SeriesList:     charts.NewSeriesListBar(values(spec, horizontal), charts.BarSeriesOption{Label: pointLabel(spec)}),
		ValueFormatter: formatValue,
	}
	if spec.Stacked {
		opt.StackSeries = charts.Ptr(true)
	}
	if len(spec.Series) > 1 {
		opt.Legend = f.legend(seriesNames(spec))
	}

	return opt
}

// pieOption labels every slice with its name, value and exact rounded share;
// a legend would only repeat the labels.
func (f fitter) pieOption(spec Spec) charts.PieChartOption {
	var total float64
	for _, v := range spec.Series[0].Values {
		total += v
	}
	names := f.fitAll(spec.Categories, 0, labelFontSize, f.width/5, false)
	label := charts.SeriesLabel{
		Show: charts.Ptr(true),
		LabelFormatter: func(_ int, name string, val float64) (string, *charts.LabelStyle) {
			return fmt.Sprintf("%s: %s (%s%%)", name, formatValue(val), strconv.FormatFloat(math.Round(val/total*1000)/10, 'f', -1, 64)), nil
		},
	}

	return charts.PieChartOption{
		Theme:      theme(len(spec.Categories)),
		Padding:    f.padding(spec),
		Legend:     charts.LegendOption{Show: charts.Ptr(false)},
		SeriesList: charts.NewSeriesListPie(spec.Series[0].Values, charts.PieSeriesOption{Label: label, Names: names}),
	}
}
