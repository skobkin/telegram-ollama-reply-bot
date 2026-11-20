// Package chart renders model-supplied structured data into PNG charts
// locally and in memory. It is deliberately independent of the statistics
// reporting code: no plotting code, templates or external services are ever
// executed on behalf of a caller.
package chart

import (
	"fmt"
	"math"
	"unicode"
	"unicode/utf8"
)

// Type selects the chart kind.
type Type string

const (
	TypeLine Type = "line"
	TypeBar  Type = "bar"
	TypePie  Type = "pie"
)

const (
	OrientationVertical   = "vertical"
	OrientationHorizontal = "horizontal"
)

// Bounds enforced by Validate. They are part of the model-facing contract.
const (
	MaxTitleRunes = 60
	MaxLabelRunes = 32
	MaxCategories = 50
	MaxSeries     = 8
	MaxPoints     = 400
	MaxPieSlices  = 12
	MaxAbsValue   = 1e15
	MinWidth      = 600
	MaxWidth      = 1600
	MinHeight     = 360
	MaxHeight     = 1200
	DefaultWidth  = 1000
	DefaultHeight = 600
)

// Series is one named run of values, one value per category.
type Series struct {
	Name   string
	Values []float64
}

// Spec is the complete, validated description of a chart.
type Spec struct {
	Type        Type
	Title       string
	Categories  []string
	Series      []Series
	Orientation string // bar only; "" means vertical
	Stacked     bool   // bar only
	XLabel      string // category axis title
	YLabel      string // value axis title
	Width       int    // 0 = DefaultWidth
	Height      int    // 0 = DefaultHeight
}

// ValidationError is a model-safe, actionable description of a rejected spec.
type ValidationError struct{ Message string }

func (e *ValidationError) Error() string { return e.Message }

func invalid(format string, args ...any) error {
	return &ValidationError{Message: fmt.Sprintf(format, args...)}
}

func checkText(field, value string, limit int, required bool) error {
	if required && value == "" {
		return invalid("%s must not be empty", field)
	}
	if utf8.RuneCountInString(value) > limit {
		return invalid("%s must be at most %d characters", field, limit)
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return invalid("%s must not contain control characters", field)
		}
	}

	return nil
}

// Validate checks every bound and chart-specific requirement.
func (s Spec) Validate() error {
	if err := s.validateShape(); err != nil {
		return err
	}
	if err := s.validateValues(); err != nil {
		return err
	}

	return s.validateKind()
}

func (s Spec) validateShape() error {
	switch s.Type {
	case TypeLine, TypeBar, TypePie:
	default:
		return invalid("type must be one of line, bar, pie")
	}
	for _, check := range []error{
		checkText("title", s.Title, MaxTitleRunes, false),
		checkText("x_label", s.XLabel, MaxLabelRunes, false),
		checkText("y_label", s.YLabel, MaxLabelRunes, false),
	} {
		if check != nil {
			return check
		}
	}
	if len(s.Categories) < 1 || len(s.Categories) > MaxCategories {
		return invalid("categories must contain 1 to %d labels", MaxCategories)
	}
	for _, category := range s.Categories {
		if err := checkText("category labels", category, MaxLabelRunes, true); err != nil {
			return err
		}
	}
	if len(s.Series) < 1 || len(s.Series) > MaxSeries {
		return invalid("series must contain 1 to %d entries", MaxSeries)
	}
	if len(s.Series)*len(s.Categories) > MaxPoints {
		return invalid("at most %d data points are allowed (series x categories)", MaxPoints)
	}
	if (s.Width != 0 && (s.Width < MinWidth || s.Width > MaxWidth)) ||
		(s.Height != 0 && (s.Height < MinHeight || s.Height > MaxHeight)) {
		return invalid("width must be %d-%d and height %d-%d", MinWidth, MaxWidth, MinHeight, MaxHeight)
	}

	return nil
}

func (s Spec) validateValues() error {
	for _, series := range s.Series {
		if err := checkText("series names", series.Name, MaxLabelRunes, true); err != nil {
			return err
		}
		if len(series.Values) != len(s.Categories) {
			return invalid("series %q has %d values but there are %d categories", series.Name, len(series.Values), len(s.Categories))
		}
		for _, value := range series.Values {
			if math.IsNaN(value) || math.IsInf(value, 0) || math.Abs(value) > MaxAbsValue {
				return invalid("values must be finite numbers with absolute value at most 1e15")
			}
		}
	}

	return nil
}

func (s Spec) validateKind() error {
	if s.Type != TypeBar && (s.Orientation != "" || s.Stacked) {
		return invalid("orientation and stacked apply to bar charts only")
	}
	switch s.Orientation {
	case "", OrientationVertical, OrientationHorizontal:
	default:
		return invalid("orientation must be vertical or horizontal")
	}
	switch s.Type {
	case TypeBar:
		if s.Stacked {
			for _, series := range s.Series {
				for _, value := range series.Values {
					if value < 0 {
						return invalid("stacked bars require non-negative values")
					}
				}
			}
		}
	case TypePie:
		if len(s.Series) != 1 {
			return invalid("pie charts take exactly one series")
		}
		if len(s.Categories) > MaxPieSlices {
			return invalid("pie charts support at most %d slices", MaxPieSlices)
		}
		var sum float64
		for _, value := range s.Series[0].Values {
			if value < 0 {
				return invalid("pie values must be non-negative")
			}
			sum += value
		}
		if sum <= 0 {
			return invalid("pie values must have a positive sum")
		}
	}

	return nil
}

func (s Spec) size() (int, int) {
	width, height := s.Width, s.Height
	if width == 0 {
		width = DefaultWidth
	}
	if height == 0 {
		height = DefaultHeight
	}

	return width, height
}
