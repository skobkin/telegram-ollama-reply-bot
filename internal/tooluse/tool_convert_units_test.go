package tooluse

import (
	"context"
	"encoding/json"
	"math"
	"strings"
	"testing"
)

func convertOK(t *testing.T, value float64, from, to string) float64 {
	t.Helper()

	result := convertUnits(value, from, to)
	if result.Status != "ok" {
		t.Fatalf("convert %v %s -> %s: unexpected failure: %+v", value, from, to, result.Error)
	}

	return result.Data.(map[string]any)["value"].(float64)
}

func convertErrorCode(t *testing.T, value float64, from, to string) string {
	t.Helper()

	result := convertUnits(value, from, to)
	if result.Status != "error" || result.Error == nil {
		t.Fatalf("convert %v %s -> %s: expected error, got %+v", value, from, to, result)
	}
	if result.Error.Class != ErrorArguments || !result.Error.Retryable {
		t.Fatalf("convert %v %s -> %s: error must be a retryable arguments error, got %+v", value, from, to, result.Error)
	}

	return result.Error.Code
}

func TestConvertUnitsValues(t *testing.T) {
	t.Parallel()

	tests := []struct {
		value    float64
		from, to string
		want     float64
	}{
		{1, "km", "m", 1000},
		{1, "mi", "km", 1.609344},
		{12, "in", "ft", 1},
		{1, "nmi", "m", 1852},
		{1, "lb", "kg", 0.45359237},
		{16, "oz", "lb", 1},
		{1, "gal_us", "L", 3.785411784},
		{1, "gal_us", "qt_us", 4},
		{1, "gal_imp", "L", 4.54609},
		{100, "degC", "degF", 212},
		{0, "degC", "degF", 32},
		{-40, "degF", "degC", -40},
		{0, "K", "degC", -273.15},
		{-273.15, "degC", "K", 0},
		{98.6, "degF", "degC", 37},
		{36, "km/h", "m/s", 10},
		{60, "mph", "km/h", 96.56064},
		{1, "kn", "km/h", 1.852},
		{1, "atm", "Pa", 101325},
		{1, "bar", "kPa", 100},
		{1, "bar", "psi", 14.503773773},
		{1, "kWh", "kJ", 3600},
		{1, "kcal", "kJ", 4.184},
		{1, "eV", "J", 1.602176634e-19},
		{1, "B", "bit", 8},
		{1, "kB", "B", 1000},
		{1, "KiB", "B", 1024},
		{1, "GB", "MB", 1000},
		{1, "GiB", "MiB", 1024},
		{1, "GB", "GiB", 0.931322574615},
		{8, "Mbit", "MB", 1},
		{1, "celsius", "fahrenheit", 33.8},
		{5, " km ", "m", 5000},
	}

	for _, tt := range tests {
		got := convertOK(t, tt.value, tt.from, tt.to)
		tolerance := 1e-9 * math.Max(1, math.Abs(tt.want))
		if math.Abs(got-tt.want) > tolerance {
			t.Errorf("%v %s -> %s = %v, want %v", tt.value, tt.from, tt.to, got, tt.want)
		}
	}
}

func TestConvertUnitsTrimsFloatNoise(t *testing.T) {
	t.Parallel()

	// 0 degC in degF is 31.999999999999996 without significant-digit rounding.
	result := convertUnits(0, "degC", "degF")
	data := result.Data.(map[string]any)
	if data["formatted"] != "32" {
		t.Fatalf("formatted = %v, want 32", data["formatted"])
	}
}

func TestConvertUnitsResultFields(t *testing.T) {
	t.Parallel()

	result := convertUnits(1, "feet", "m")
	data := result.Data.(map[string]any)
	if data["from"] != "ft" || data["to"] != "m" || data["dimension"] != dimLength {
		t.Fatalf("unexpected canonical fields: %+v", data)
	}
}

func TestConvertUnitsRejections(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		value    float64
		from, to string
		want     string
	}{
		{"unknown source", 1, "parsec", "m", "unknown_unit"},
		{"unknown target", 1, "m", "parsec", "unknown_unit"},
		{"empty unit", 1, "", "m", "unknown_unit"},
		{"case sensitive", 1, "KM", "m", "unknown_unit"},
		{"ambiguous KB", 1, "KB", "B", "unknown_unit"},
		{"ambiguous Kb", 1, "Kb", "B", "unknown_unit"},
		{"ambiguous gb", 1, "gb", "B", "unknown_unit"},
		{"currency", 1, "USD", "EUR", "unknown_unit"},
		{"length to mass", 1, "km", "kg", "incompatible_units"},
		{"decimal data to energy", 1, "kB", "kJ", "incompatible_units"},
		{"below absolute zero degC", -300, "degC", "K", "below_absolute_zero"},
		{"below absolute zero K", -1, "K", "degC", "below_absolute_zero"},
		{"below absolute zero degF", -500, "degF", "degC", "below_absolute_zero"},
		{"NaN", math.NaN(), "m", "km", "invalid_value"},
		{"infinity", math.Inf(1), "m", "km", "invalid_value"},
		{"overflow", 1e308, "km", "mm", "result_out_of_range"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := convertErrorCode(t, tt.value, tt.from, tt.to); got != tt.want {
				t.Fatalf("code = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestConvertUnitsAbsoluteZeroIsAllowed(t *testing.T) {
	t.Parallel()

	if got := convertOK(t, -273.15, "degC", "degF"); math.Abs(got-(-459.67)) > 1e-9 {
		t.Fatalf("absolute zero in degF = %v, want -459.67", got)
	}
}

func TestConvertUnitsResultSizeStaysWithinBudget(t *testing.T) {
	t.Parallel()

	ok := convertUnits(1, "km", "mi")
	if strings.Contains(marshalResult(ok, convertUnitsResultCharBudget), "\"1\"") {
		t.Fatal("successful result must not echo the input value")
	}
	if _, prep := prepareResult(ok, convertUnitsResultCharBudget); prep != ResultPreparationComplete {
		t.Fatalf("preparation = %s", prep)
	}

	hostile := strings.Repeat("<\"\\", 200)
	for _, result := range []toolResult{
		convertUnits(1, hostile, "m"),
		convertUnits(1, "m", hostile),
	} {
		text := marshalResult(result, convertUnitsResultCharBudget)
		if len([]rune(text)) > convertUnitsResultCharBudget {
			t.Fatalf("result exceeds budget: %d", len([]rune(text)))
		}
		if _, prep := prepareResult(result, convertUnitsResultCharBudget); prep == ResultPreparationFailed {
			t.Fatal("hostile unit error failed delivery")
		}
		if strings.Contains(result.Error.Message, hostile) {
			t.Fatal("error must truncate echoed unit strings")
		}
	}
}

func TestConvertUnitsHandler(t *testing.T) {
	t.Parallel()

	result, err := convertUnitsHandler(context.Background(), CallContext{}, json.RawMessage(`{"value":2,"from":"km","to":"m"}`))
	if err != nil || result.Status != "ok" {
		t.Fatalf("valid call failed: %+v %v", result, err)
	}
	if got := result.Data.(map[string]any)["value"]; got != 2000.0 {
		t.Fatalf("value = %v, want 2000", got)
	}

	// A semantic failure is a structured error result, not a handler error.
	result, err = convertUnitsHandler(context.Background(), CallContext{}, json.RawMessage(`{"value":1,"from":"km","to":"kg"}`))
	if err != nil || result.Error == nil || result.Error.Code != "incompatible_units" {
		t.Fatalf("incompatible units: %+v %v", result, err)
	}

	for _, raw := range []string{
		`{}`,
		`{"from":"km","to":"m"}`,
		`{"value":"1","from":"km","to":"m"}`,
		`{"value":1,"from":"km","to":"m","x":2}`,
		`[]`,
	} {
		if _, err := convertUnitsHandler(context.Background(), CallContext{}, json.RawMessage(raw)); err == nil {
			t.Errorf("%s: expected invalid arguments error", raw)
		}
	}
}

func TestConvertUnitsTableIntegrity(t *testing.T) {
	t.Parallel()

	dimensions := map[string]bool{}
	for _, dimension := range convertUnitsDimensions {
		dimensions[dimension] = false
	}

	seen := map[string]bool{}
	for _, unit := range convertUnitsTable {
		if seen[unit.id] {
			t.Errorf("duplicate unit id %q", unit.id)
		}
		seen[unit.id] = true

		if _, ok := dimensions[unit.dimension]; !ok {
			t.Errorf("unit %q has unlisted dimension %q", unit.id, unit.dimension)
		}
		dimensions[unit.dimension] = true

		if unit.factor <= 0 || math.IsInf(unit.factor, 0) || math.IsNaN(unit.factor) {
			t.Errorf("unit %q has invalid factor %v", unit.id, unit.factor)
		}
	}
	for dimension, used := range dimensions {
		if !used {
			t.Errorf("dimension %q has no units", dimension)
		}
	}

	for alias, id := range convertUnitsAliases {
		if seen[alias] {
			t.Errorf("alias %q shadows a canonical unit id", alias)
		}
		if !seen[id] {
			t.Errorf("alias %q points at unknown unit %q", alias, id)
		}
	}
}

func TestConvertUnitsDescriptionListsEveryUnit(t *testing.T) {
	t.Parallel()

	description := convertUnitsDescription()
	for _, unit := range convertUnitsTable {
		if !strings.Contains(description, " "+unit.id) && !strings.Contains(description, ": "+unit.id) {
			t.Errorf("description does not list unit %q", unit.id)
		}
	}
	for _, want := range []string{"kB", "KiB", "case-sensitive", "ambiguous"} {
		if !strings.Contains(description, want) {
			t.Errorf("description missing %q", want)
		}
	}
}
