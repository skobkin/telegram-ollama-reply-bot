package chart

import (
	"errors"
	"math"
	"strings"
	"testing"
)

func validBar() Spec {
	return Spec{Type: TypeBar, Categories: []string{"a", "b"}, Series: []Series{{"s", []float64{1, 2}}}}
}

func TestValidateAcceptsValidSpecs(t *testing.T) {
	for name, spec := range sampleSpecs() {
		if err := spec.Validate(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestValidateRejections(t *testing.T) {
	long := strings.Repeat("x", MaxLabelRunes+1)
	cases := map[string]func(*Spec){
		"bad type":              func(s *Spec) { s.Type = "scatter" },
		"empty type":            func(s *Spec) { s.Type = "" },
		"long title":            func(s *Spec) { s.Title = strings.Repeat("t", MaxTitleRunes+1) },
		"control in title":      func(s *Spec) { s.Title = "a\nb" },
		"long x label":          func(s *Spec) { s.XLabel = long },
		"long y label":          func(s *Spec) { s.YLabel = long },
		"no categories":         func(s *Spec) { s.Categories = nil; s.Series[0].Values = nil },
		"too many categories":   func(s *Spec) { s.Categories = make([]string, MaxCategories+1) },
		"empty category":        func(s *Spec) { s.Categories[0] = "" },
		"long category":         func(s *Spec) { s.Categories[0] = long },
		"no series":             func(s *Spec) { s.Series = nil },
		"too many series":       func(s *Spec) { s.Series = make([]Series, MaxSeries+1) },
		"empty series name":     func(s *Spec) { s.Series[0].Name = "" },
		"long series name":      func(s *Spec) { s.Series[0].Name = long },
		"length mismatch":       func(s *Spec) { s.Series[0].Values = []float64{1} },
		"nan":                   func(s *Spec) { s.Series[0].Values[0] = math.NaN() },
		"inf":                   func(s *Spec) { s.Series[0].Values[0] = math.Inf(1) },
		"too large":             func(s *Spec) { s.Series[0].Values[0] = 2e15 },
		"too small":             func(s *Spec) { s.Series[0].Values[0] = -2e15 },
		"narrow":                func(s *Spec) { s.Width = MinWidth - 1 },
		"wide":                  func(s *Spec) { s.Width = MaxWidth + 1 },
		"short":                 func(s *Spec) { s.Height = MinHeight - 1 },
		"tall":                  func(s *Spec) { s.Height = MaxHeight + 1 },
		"bad orientation":       func(s *Spec) { s.Orientation = "diagonal" },
		"stacked negative":      func(s *Spec) { s.Stacked = true; s.Series[0].Values[0] = -1 },
		"orientation on line":   func(s *Spec) { s.Type = TypeLine; s.Orientation = OrientationHorizontal },
		"stacked on line":       func(s *Spec) { s.Type = TypeLine; s.Stacked = true },
		"pie two series":        func(s *Spec) { s.Type = TypePie; s.Series = append(s.Series, Series{"t", []float64{1, 1}}) },
		"pie negative":          func(s *Spec) { s.Type = TypePie; s.Series[0].Values[0] = -1 },
		"pie zero sum":          func(s *Spec) { s.Type = TypePie; s.Series[0].Values = []float64{0, 0} },
		"pie too many slices":   func(s *Spec) { pieOfSize(s, MaxPieSlices+1) },
		"too many data points":  func(s *Spec) { grid(s, MaxCategories, MaxSeries+1) },
		"points over the limit": func(s *Spec) { grid(s, 50, 8); s.Categories = append(s.Categories, "x"); extend(s) },
	}
	for name, mutate := range cases {
		spec := validBar()
		mutate(&spec)
		err := spec.Validate()
		var verr *ValidationError
		if err == nil || !errors.As(err, &verr) || verr.Message == "" {
			t.Errorf("%s: expected a ValidationError, got %v", name, err)
		}
	}
}

func pieOfSize(s *Spec, n int) {
	s.Type = TypePie
	s.Categories = make([]string, n)
	values := make([]float64, n)
	for i := range s.Categories {
		s.Categories[i], values[i] = "c", 1
	}
	s.Series = []Series{{"s", values}}
}

func grid(s *Spec, cats, series int) {
	s.Categories = make([]string, cats)
	for i := range s.Categories {
		s.Categories[i] = "c"
	}
	s.Series = make([]Series, series)
	for i := range s.Series {
		s.Series[i] = Series{"s", make([]float64, cats)}
	}
}

func extend(s *Spec) {
	for i := range s.Series {
		s.Series[i].Values = append(s.Series[i].Values, 0)
	}
}

func TestMaximumPointCountIsAccepted(t *testing.T) {
	spec := validBar()
	grid(&spec, 50, 8)
	if err := spec.Validate(); err != nil {
		t.Fatalf("50 x 8 = 400 points must be accepted: %v", err)
	}
	if _, err := Render(spec); err != nil {
		t.Fatalf("maximum chart must render: %v", err)
	}
}

func TestRenderValidatesFirst(t *testing.T) {
	spec := validBar()
	spec.Series[0].Values[0] = math.NaN()
	_, err := Render(spec)
	var verr *ValidationError
	if !errors.As(err, &verr) {
		t.Fatalf("expected ValidationError, got %v", err)
	}
}

func TestRecoverRenderTurnsPanicsIntoErrRender(t *testing.T) {
	png, err := recoverRender(func() ([]byte, error) { panic("library boom: secret detail") })
	if png != nil || !errors.Is(err, ErrRender) || strings.Contains(err.Error(), "secret") {
		t.Fatalf("got %v %v", png, err)
	}
}

func TestFormatValue(t *testing.T) {
	cases := map[float64]string{0: "0", 0.0025: "0.0025", 12.3456: "12.35", -50: "-50", 120: "120", 1500: "1.5k"}
	for in, want := range cases {
		if got := formatValue(in); got != want {
			t.Errorf("formatValue(%v) = %q, want %q", in, got, want)
		}
	}
}

func TestValueAxisRangeKeepsZeroTickAndHeadroom(t *testing.T) {
	spec := Spec{Type: TypeBar, Categories: []string{"a", "b"}, Series: []Series{{"s", []float64{-50, 80}}}}
	lo, hi, step := valueAxisRange(spec)
	if lo > -50 || hi < 80 || math.Mod(lo, step) != 0 || math.Mod(hi, step) != 0 || lo >= 0 || hi <= 0 {
		t.Fatalf("range = %v..%v step %v", lo, hi, step)
	}
	if lo, hi, _ := valueAxisRange(Spec{Type: TypeLine, Categories: []string{"a"}, Series: []Series{{"s", []float64{0}}}}); lo != 0 || hi != 1 {
		t.Errorf("all-zero range = %v..%v", lo, hi)
	}
	if lo, hi, _ := valueAxisRange(Spec{Type: TypeLine, Categories: []string{"a"}, Series: []Series{{"s", []float64{5}}}}); lo >= 5 || hi <= 5 {
		t.Errorf("constant range = %v..%v", lo, hi)
	}
	stacked := Spec{Type: TypeBar, Stacked: true, Categories: []string{"a"}, Series: []Series{{"x", []float64{3}}, {"y", []float64{4}}}}
	if _, hi, _ := valueAxisRange(stacked); hi < 7 {
		t.Errorf("stacked range must cover the sum: hi = %v", hi)
	}
}
