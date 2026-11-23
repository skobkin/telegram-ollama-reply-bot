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
		{32, "degF", "degC", 0},
		{1, "TB", "B", 1e12},
		{1, "TiB", "B", 1099511627776},
		{1, "PB", "B", 1e15},
		{1, "PiB", "B", 1125899906842624},
	}

	for _, tt := range tests {
		got := convertOK(t, tt.value, tt.from, tt.to)
		// Relative only, no absolute floor: tiny wants (eV) stay checked and
		// want == 0 must be exact.
		tolerance := 1e-12 * math.Abs(tt.want)
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
	// A distinctive value proves that no result path echoes the input: a
	// numeric echo would marshal as 12345.678.
	for _, result := range []toolResult{
		convertUnits(12345.678, "km", "mi"),
		convertUnits(12345.678, "km", "kg"),
		convertUnits(12345.678, "parsec", "m"),
		convertUnits(-12345.678, "degC", "K"),
	} {
		if text := marshalResult(result, convertUnitsResultCharBudget); strings.Contains(text, "12345.678") {
			t.Fatalf("result must not echo the input value: %s", text)
		}
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
	// Parse "Units: dimension: id id; dimension: id id." and compare whole ids:
	// a substring check matches longer ids (m in mm) and the prose before the
	// listing (in, kB, KiB).
	_, listing, ok := strings.Cut(description, " Units: ")
	if !ok {
		t.Fatalf("description has no unit listing: %q", description)
	}
	listed := map[string]string{}
	for _, group := range strings.Split(strings.TrimSuffix(listing, "."), "; ") {
		dimension, ids, _ := strings.Cut(group, ": ")
		for _, id := range strings.Fields(ids) {
			listed[id] = dimension
		}
	}
	for _, unit := range convertUnitsTable {
		if listed[unit.id] != unit.dimension {
			t.Errorf("description lists unit %q under %q, want %q", unit.id, listed[unit.id], unit.dimension)
		}
	}
	if len(listed) != len(convertUnitsTable) {
		t.Errorf("description lists %d units, table has %d", len(listed), len(convertUnitsTable))
	}
	for _, want := range []string{"kB", "KiB", "case-sensitive", "ambiguous", "absolute readings, not differences"} {
		if !strings.Contains(description, want) {
			t.Errorf("description missing %q", want)
		}
	}
}

// Exact integer results must stay exact: 1 TiB is 1099511627776 B (13 digits),
// which twelve-significant-digit rounding turned into 1099511627780. The
// shared 1e-9 relative tolerance of TestConvertUnitsValues cannot see that, so
// these compare exactly.
func TestConvertUnitsKeepsLargeIntegersExact(t *testing.T) {
	t.Parallel()

	tests := []struct {
		value    float64
		from, to string
		want     float64
		wantText string
	}{
		{1, "TiB", "B", 1099511627776, "1099511627776"},
		{4, "TiB", "B", 4398046511104, "4398046511104"},
		{1, "PiB", "B", 1125899906842624, "1125899906842624"},
		{1, "PiB", "KiB", 1099511627776, "1099511627776"},
		{1099511627777, "B", "B", 1099511627777, "1099511627777"},
		{1, "GiB", "B", 1073741824, "1073741824"},
		// Beyond 2^53 data sizes stay exact too: their factors lose nothing.
		{2, "PiB", "bit", 18014398509481984, "18014398509481984"},
		{10, "PiB", "B", 11258999068426240, "11258999068426240"},
		// Decimal factors do leave noise there, which rounding still hides.
		{1e10, "km", "mm", 1e16, "10000000000000000"},
	}

	for _, tt := range tests {
		data := convertUnits(tt.value, tt.from, tt.to).Data.(map[string]any)
		if got := data["value"].(float64); got != tt.want {
			t.Errorf("%v %s -> %s = %v, want exactly %v", tt.value, tt.from, tt.to, got, tt.want)
		}
		if data["formatted"] != tt.wantText {
			t.Errorf("%v %s -> %s formatted = %v, want %s", tt.value, tt.from, tt.to, data["formatted"], tt.wantText)
		}
	}
}

// An offset conversion subtracts terms as large as the offsets, so the float64
// residue of an exact zero is absolute noise (5.68e-14 for 32 degF -> degC)
// that significant-digit rounding cannot remove.
func TestConvertUnitsSnapsCancellationResidueToZero(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		value    float64
		from, to string
		want     float64
	}{
		{32, "degF", "degC", 0},
		{273.15, "K", "degC", 0},
		{-459.67, "degF", "K", 0},
		{-17.77777777777778, "degC", "degF", 0},
		{0, "degC", "degF", 32},
		{100, "degC", "degF", 212},
	} {
		if got := convertOK(t, tt.value, tt.from, tt.to); got != tt.want {
			t.Errorf("%v %s -> %s = %v, want exactly %v", tt.value, tt.from, tt.to, got, tt.want)
		}
	}

	// A real (if tiny) difference must survive: only noise is snapped.
	if got := convertOK(t, 273.15+1e-9, "K", "degC"); got == 0 || math.Abs(got-1e-9) > 1e-12 {
		t.Fatalf("a 1e-9 K difference must not be snapped away, got %v", got)
	}
}

// The display string is plain decimal digits, never exponent notation.
func TestConvertUnitsFormattedIsPlainDecimal(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		value    float64
		from, to string
		want     string
	}{
		{1500, "km", "m", "1500000"},
		{1, "GiB", "B", "1073741824"},
		{8, "GB", "bit", "64000000000"},
		{1, "mm", "km", "0.000001"},
		{1, "mi", "km", "1.609344"},
		// Beyond the readable range the compact exponent form is kept.
		{1, "eV", "J", "1.602176634e-19"},
		{1e22, "m", "m", "1e+22"},
	} {
		data := convertUnits(tt.value, tt.from, tt.to).Data.(map[string]any)
		if data["formatted"] != tt.want {
			t.Errorf("%v %s -> %s formatted = %v, want %s", tt.value, tt.from, tt.to, data["formatted"], tt.want)
		}
	}
}

// A temperature within the absolute-zero tolerance is the noise of an input at
// absolute zero: it is accepted, and reported as 0 K, never a negative kelvin.
func TestConvertUnitsNeverReportsNegativeKelvin(t *testing.T) {
	t.Parallel()

	if got := convertOK(t, -273.15000000001, "degC", "K"); got != 0 {
		t.Fatalf("-273.15000000001 degC in K = %v, want 0", got)
	}
	if got := convertOK(t, -273.15000000001, "degC", "degF"); math.Abs(got-(-459.67)) > 1e-9 {
		t.Fatalf("-273.15000000001 degC in degF = %v, want -459.67", got)
	}
	if got := convertErrorCode(t, -273.151, "degC", "K"); got != "below_absolute_zero" {
		t.Fatalf("a real temperature below absolute zero must still be rejected, got %q", got)
	}
}
