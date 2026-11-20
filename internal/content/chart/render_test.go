package chart

import (
	"bytes"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func vals(n int, f func(i int) float64) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = f(i)
	}

	return out
}

func cats(n int, prefix string) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = prefix + string(rune('A'+i%26)) + strings.Repeat("x", i%3)
	}

	return out
}

// sampleSpecs are representative charts: they are rendered and decoded by
// the tests, and written to CHART_SAMPLE_DIR for visual inspection.
func sampleSpecs() map[string]Spec {
	return map[string]Spec{
		"line_multi_negative": {Type: TypeLine, Title: "Temperature by month", Categories: []string{"Jan", "Feb", "Mar", "Apr", "May", "Jun"},
			Series: []Series{{"Oslo", []float64{-7, -6, -2, 4, 10, 15}}, {"Cairo", []float64{14, 16, 19, 24, 29, 32}}}, XLabel: "Month", YLabel: "°C"},
		"bar_vertical": {Type: TypeBar, Title: "Sales", Categories: []string{"Q1", "Q2", "Q3", "Q4"},
			Series: []Series{{"2025", []float64{120, 150, 90, 200}}, {"2026", []float64{130, 170, 110, 240}}}, XLabel: "Quarter", YLabel: "Units"},
		"bar_horizontal": {Type: TypeBar, Title: "Votes", Orientation: OrientationHorizontal, Categories: []string{"Pizza", "Sushi", "Burgers", "Salad"},
			Series: []Series{{"Votes", []float64{12, 9, 7, 3}}}, XLabel: "Food", YLabel: "Votes"},
		"bar_stacked": {Type: TypeBar, Title: "Stacked", Stacked: true, Categories: []string{"Mon", "Tue", "Wed", "Thu"},
			Series: []Series{{"Done", []float64{5, 7, 3, 8}}, {"Failed", []float64{1, 0, 2, 1}}, {"Skipped", []float64{2, 2, 2, 2}}}},
		"bar_horizontal_stacked": {Type: TypeBar, Title: "Horizontal stacked", Stacked: true, Orientation: OrientationHorizontal, Categories: []string{"alpha", "beta", "gamma"},
			Series: []Series{{"A", []float64{5, 7, 3}}, {"B", []float64{1, 4, 2}}}},
		"bar_negative": {Type: TypeBar, Title: "Profit and loss", Categories: []string{"Q1", "Q2", "Q3", "Q4"},
			Series: []Series{{"P&L", []float64{-50, 30, -10, 80}}}},
		"line_all_zero": {Type: TypeLine, Title: "All zero", Categories: []string{"a", "b", "c"}, Series: []Series{{"zero", []float64{0, 0, 0}}}},
		"bar_all_zero":  {Type: TypeBar, Title: "All zero bars", Categories: []string{"a", "b", "c"}, Series: []Series{{"zero", []float64{0, 0, 0}}}},
		"line_constant": {Type: TypeLine, Title: "Constant", Categories: []string{"a", "b", "c"}, Series: []Series{{"c", []float64{5, 5, 5}}}},
		"line_dense_50": {Type: TypeLine, Title: "Dense line", Categories: cats(50, "d"), Series: []Series{{"s", vals(50, func(i int) float64 { return math.Sin(float64(i)/5) * 100 })}}},
		"bar_dense_50":  {Type: TypeBar, Title: "Dense bars", Categories: cats(50, "c"), Series: []Series{{"s", vals(50, func(i int) float64 { return float64(i%7) * 3 })}}},
		"bar_dense_horizontal": {Type: TypeBar, Title: "Dense horizontal", Orientation: OrientationHorizontal, Categories: cats(30, "row"),
			Series: []Series{{"s", vals(30, func(i int) float64 { return float64(i + 1) })}}},
		"pie":            {Type: TypePie, Title: "Share", Categories: []string{"Go", "Rust", "Python", "Other"}, Series: []Series{{"share", []float64{40, 25, 20, 15}}}},
		"pie_zero_slice": {Type: TypePie, Title: "Zero slice", Categories: []string{"A", "B", "C"}, Series: []Series{{"s", []float64{10, 0, 5}}}},
		"pie_12":         {Type: TypePie, Title: "Twelve slices", Categories: cats(12, "slice"), Series: []Series{{"s", vals(12, func(i int) float64 { return float64(i + 1) })}}},
		"cyrillic_bar": {Type: TypeBar, Title: "Продажи по кварталам", Categories: []string{"Первый", "Второй", "Третий"},
			Series: []Series{{"Выручка, ₽", []float64{120, 90, 150}}, {"Расходы", []float64{80, 70, 60}}}, XLabel: "Квартал", YLabel: "Тысяч рублей"},
		"cyrillic_pie":  {Type: TypePie, Title: "Любимые языки", Categories: []string{"Го", "Раст", "Питон"}, Series: []Series{{"доля", []float64{50, 30, 20}}}},
		"cyrillic_line": {Type: TypeLine, Title: "Курс", Categories: []string{"янв", "фев", "мар"}, Series: []Series{{"Доллар", []float64{90, 92, 95}}, {"Евро", []float64{98, 99, 101}}}},
		"min_size":      {Type: TypeBar, Title: "Min", Width: MinWidth, Height: MinHeight, Categories: []string{"a", "b"}, Series: []Series{{"s", []float64{1, 2}}}},
		"max_size":      {Type: TypeLine, Title: "Max", Width: MaxWidth, Height: MaxHeight, Categories: []string{"a", "b", "c"}, Series: []Series{{"s", []float64{3, 1, 2}}}},
		"large_values":  {Type: TypeBar, Title: "Large", Categories: []string{"a", "b"}, Series: []Series{{"s", []float64{1e15, 5e14}}}},
		"long_labels": {Type: TypeBar, Title: strings.Repeat("W", MaxTitleRunes), Categories: []string{strings.Repeat("L", MaxLabelRunes), strings.Repeat("M", MaxLabelRunes)},
			Series: []Series{{strings.Repeat("N", MaxLabelRunes), []float64{3, 4}}}, XLabel: strings.Repeat("X", MaxLabelRunes), YLabel: strings.Repeat("Y", MaxLabelRunes)},
		"long_labels_min_size": {Type: TypeBar, Title: strings.Repeat("W", MaxTitleRunes), Width: MinWidth, Height: MinHeight,
			Categories: []string{strings.Repeat("L", MaxLabelRunes), strings.Repeat("M", MaxLabelRunes), strings.Repeat("K", MaxLabelRunes)},
			Series:     []Series{{strings.Repeat("N", MaxLabelRunes), []float64{3, 4, 2}}, {"b", []float64{1, 2, 3}}}, XLabel: strings.Repeat("X", MaxLabelRunes), YLabel: strings.Repeat("Y", MaxLabelRunes)},
		"single_point": {Type: TypeLine, Title: "One point", Categories: []string{"only"}, Series: []Series{{"s", []float64{4}}}},
		"fractional":   {Type: TypeBar, Title: "Fractions", Categories: []string{"a", "b", "c"}, Series: []Series{{"s", []float64{0.001, 0.0025, 0.004}}}},
	}
}

func TestSampleSpecsRenderDecodableSizedPNGs(t *testing.T) {
	dir := os.Getenv("CHART_SAMPLE_DIR")
	for name, spec := range sampleSpecs() {
		data, err := Render(spec)
		if err != nil {
			t.Errorf("%s: %v", name, err)

			continue
		}
		img, err := png.Decode(bytes.NewReader(data))
		if err != nil {
			t.Errorf("%s: not a PNG: %v", name, err)

			continue
		}
		wantW, wantH := spec.size()
		if b := img.Bounds(); b.Dx() != wantW || b.Dy() != wantH {
			t.Errorf("%s: size %dx%d, want %dx%d", name, b.Dx(), b.Dy(), wantW, wantH)
		}
		if dir != "" {
			if err := os.WriteFile(filepath.Join(dir, name+".png"), data, 0o600); err != nil { //nolint:gosec // opt-in test output directory from the environment
				t.Fatal(err)
			}
		}
	}
}
