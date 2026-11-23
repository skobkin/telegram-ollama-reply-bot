package tooluse

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"
)

const convertUnitsMaxUnitRunes = 32

// absoluteZeroTolerance absorbs float64 noise when a temperature lands
// exactly on absolute zero (for example -273.15 degC).
const absoluteZeroTolerance = 1e-9

const (
	dimLength      = "length"
	dimMass        = "mass"
	dimVolume      = "volume"
	dimTemperature = "temperature"
	dimSpeed       = "speed"
	dimPressure    = "pressure"
	dimEnergy      = "energy"
	dimData        = "data"
)

// convertUnitsDimensions fixes the order of dimensions in the tool
// description and in error hints.
var convertUnitsDimensions = []string{
	dimLength, dimMass, dimVolume, dimTemperature, dimSpeed, dimPressure, dimEnergy, dimData,
}

// unitDef describes one unit relative to its dimension's base unit:
// base = value*factor + offset. Only temperature units use a non-zero offset.
type unitDef struct {
	id        string
	dimension string
	factor    float64
	offset    float64
}

const (
	litersPerUSGallon = 3.785411784
	joulesPerCalorie  = 4.184
	bitsPerByte       = 8
)

// convertUnitsTable lists every supported unit. Base units: m, kg, L, K, m/s,
// Pa, J and bit. IDs are case-sensitive and unambiguous; data sizes keep
// decimal (kB, MB, ...) and binary (KiB, MiB, ...) prefixes apart.
var convertUnitsTable = []unitDef{
	{"mm", dimLength, 0.001, 0},
	{"cm", dimLength, 0.01, 0},
	{"m", dimLength, 1, 0},
	{"km", dimLength, 1000, 0},
	{"in", dimLength, 0.0254, 0},
	{"ft", dimLength, 0.3048, 0},
	{"yd", dimLength, 0.9144, 0},
	{"mi", dimLength, 1609.344, 0},
	{"nmi", dimLength, 1852, 0},

	{"mg", dimMass, 1e-6, 0},
	{"g", dimMass, 1e-3, 0},
	{"kg", dimMass, 1, 0},
	{"t", dimMass, 1000, 0},
	{"oz", dimMass, 0.028349523125, 0},
	{"lb", dimMass, 0.45359237, 0},
	{"st", dimMass, 6.35029318, 0},

	{"mL", dimVolume, 0.001, 0},
	{"L", dimVolume, 1, 0},
	{"m3", dimVolume, 1000, 0},
	{"tsp_us", dimVolume, litersPerUSGallon / 768, 0},
	{"tbsp_us", dimVolume, litersPerUSGallon / 256, 0},
	{"floz_us", dimVolume, litersPerUSGallon / 128, 0},
	{"cup_us", dimVolume, litersPerUSGallon / 16, 0},
	{"pt_us", dimVolume, litersPerUSGallon / 8, 0},
	{"qt_us", dimVolume, litersPerUSGallon / 4, 0},
	{"gal_us", dimVolume, litersPerUSGallon, 0},
	{"gal_imp", dimVolume, 4.54609, 0},

	{"K", dimTemperature, 1, 0},
	{"degC", dimTemperature, 1, 273.15},
	{"degF", dimTemperature, 5.0 / 9.0, 273.15 - 32*5.0/9.0},

	{"m/s", dimSpeed, 1, 0},
	{"km/h", dimSpeed, 1 / 3.6, 0},
	{"mph", dimSpeed, 0.44704, 0},
	{"ft/s", dimSpeed, 0.3048, 0},
	{"kn", dimSpeed, 1852.0 / 3600.0, 0},

	{"Pa", dimPressure, 1, 0},
	{"kPa", dimPressure, 1e3, 0},
	{"MPa", dimPressure, 1e6, 0},
	{"mbar", dimPressure, 100, 0},
	{"bar", dimPressure, 1e5, 0},
	{"atm", dimPressure, 101325, 0},
	{"psi", dimPressure, 6894.757293168361, 0},
	{"mmHg", dimPressure, 133.322387415, 0},

	{"J", dimEnergy, 1, 0},
	{"kJ", dimEnergy, 1e3, 0},
	{"MJ", dimEnergy, 1e6, 0},
	{"cal", dimEnergy, joulesPerCalorie, 0},
	{"kcal", dimEnergy, joulesPerCalorie * 1e3, 0},
	{"Wh", dimEnergy, 3600, 0},
	{"kWh", dimEnergy, 3.6e6, 0},
	{"BTU", dimEnergy, 1055.05585262, 0},
	{"eV", dimEnergy, 1.602176634e-19, 0},

	{"bit", dimData, 1, 0},
	{"B", dimData, bitsPerByte, 0},
	{"kbit", dimData, 1e3, 0},
	{"Mbit", dimData, 1e6, 0},
	{"Gbit", dimData, 1e9, 0},
	{"kB", dimData, bitsPerByte * 1e3, 0},
	{"MB", dimData, bitsPerByte * 1e6, 0},
	{"GB", dimData, bitsPerByte * 1e9, 0},
	{"TB", dimData, bitsPerByte * 1e12, 0},
	{"PB", dimData, bitsPerByte * 1e15, 0},
	{"KiB", dimData, bitsPerByte * (1 << 10), 0},
	{"MiB", dimData, bitsPerByte * (1 << 20), 0},
	{"GiB", dimData, bitsPerByte * (1 << 30), 0},
	{"TiB", dimData, bitsPerByte * (1 << 40), 0},
	{"PiB", dimData, bitsPerByte * (1 << 50), 0},
}

// convertUnitsAliases maps a few common spellings to canonical IDs. Ambiguous
// spellings (KB, Kb, mb, gb, C, F) are deliberately absent so the caller has
// to choose, for example, between kB (1000 bytes) and KiB (1024 bytes).
var convertUnitsAliases = map[string]string{
	"°C": "degC", "celsius": "degC",
	"°F": "degF", "fahrenheit": "degF",
	"kelvin": "K",
	"l":      "L", "liter": "L", "litre": "L",
	"kmh": "km/h", "kph": "km/h",
	"knot": "kn", "knots": "kn",
	"lbs":  "lb",
	"inch": "in", "foot": "ft", "feet": "ft", "mile": "mi", "miles": "mi",
}

var convertUnitsIndex = func() map[string]unitDef {
	index := make(map[string]unitDef, len(convertUnitsTable)+len(convertUnitsAliases))
	for _, unit := range convertUnitsTable {
		index[unit.id] = unit
	}
	for alias, id := range convertUnitsAliases {
		index[alias] = index[id]
	}

	return index
}()

// convertUnitsDescription renders the supported units per dimension from the
// table, so the model-facing description cannot drift from the code.
func convertUnitsDescription() string {
	var b strings.Builder
	b.WriteString("Convert a number between physical units locally and exactly (float64), instead of converting in your head. " +
		"Units must have the same dimension. Unit ids are case-sensitive. Data sizes: kB/MB/GB/TB/PB are decimal (1000 bytes), " +
		"KiB/MiB/GiB/TiB/PiB are binary (1024 bytes); KB, Kb, mb and gb are rejected as ambiguous. Currencies are not supported. Units: ")

	for i, dimension := range convertUnitsDimensions {
		if i > 0 {
			b.WriteString("; ")
		}
		b.WriteString(dimension)
		b.WriteString(": ")

		first := true
		for _, unit := range convertUnitsTable {
			if unit.dimension != dimension {
				continue
			}
			if !first {
				b.WriteString(" ")
			}
			b.WriteString(unit.id)
			first = false
		}
	}
	b.WriteString(".")

	return b.String()
}

func convertUnitsError(code, message, hint string) toolResult {
	return ErrorResult(NewToolError(code, ErrorArguments, message, true, hint))
}

func convertUnitsHandler(_ context.Context, callCtx CallContext, args json.RawMessage) (toolResult, error) {
	var payload struct {
		Value *float64 `json:"value"`
		From  string   `json:"from"`
		To    string   `json:"to"`
	}
	if !decodeToolJSON(args, &payload) || payload.Value == nil {
		return toolResult{}, invalidArguments("Provide an object with a numeric value and from and to unit ids.")
	}

	result := convertUnits(*payload.Value, payload.From, payload.To)
	logToolResult(callCtx, result)

	return result, nil
}

// convertUnits is the pure conversion core. It never echoes the input value;
// unit strings are echoed only after truncation and quoting.
func convertUnits(value float64, from, to string) toolResult {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return convertUnitsError("invalid_value", "value must be a finite number.", "Provide a finite numeric value.")
	}

	fromUnit, ok := lookupConvertUnit(from)
	if !ok {
		return unknownUnitResult("from", from)
	}
	toUnit, ok := lookupConvertUnit(to)
	if !ok {
		return unknownUnitResult("to", to)
	}
	if fromUnit.dimension != toUnit.dimension {
		return convertUnitsError("incompatible_units",
			fmt.Sprintf("cannot convert %s (%s) to %s (%s).", fromUnit.dimension, fromUnit.id, toUnit.dimension, toUnit.id),
			"Both units must measure the same dimension; pick a target unit from the same dimension.")
	}

	base := value*fromUnit.factor + fromUnit.offset
	if toUnit.dimension == dimTemperature && base < -absoluteZeroTolerance {
		return convertUnitsError("below_absolute_zero", "temperature is below absolute zero.", "Provide a temperature at or above absolute zero (0 K, -273.15 degC).")
	}

	converted := (base - toUnit.offset) / toUnit.factor
	if math.IsNaN(converted) || math.IsInf(converted, 0) {
		return convertUnitsError("result_out_of_range", "result is outside the representable range.", "Use a smaller value or a coarser unit.")
	}
	converted = roundSignificant(converted, 12)

	return toolResult{
		Status:  "ok",
		Summary: "Converted " + fromUnit.dimension,
		Data: map[string]any{
			"value":     converted,
			"formatted": strconv.FormatFloat(converted, 'g', -1, 64),
			"from":      fromUnit.id,
			"to":        toUnit.id,
			"dimension": fromUnit.dimension,
		},
	}
}

func lookupConvertUnit(id string) (unitDef, bool) {
	unit, ok := convertUnitsIndex[strings.TrimSpace(id)]

	return unit, ok
}

func unknownUnitResult(field, raw string) toolResult {
	shown := raw
	if utf8.RuneCountInString(shown) > convertUnitsMaxUnitRunes {
		shown = string([]rune(shown)[:convertUnitsMaxUnitRunes]) + "…"
	}

	return convertUnitsError("unknown_unit",
		fmt.Sprintf("%s unit %q is not supported.", field, shown),
		"Use a unit id from the tool description. Unit ids are case-sensitive; use kB for 1000 bytes and KiB for 1024 bytes.")
}

// roundSignificant trims float64 representation noise (for example
// 31.999999999999996 for 0 degC in degF) without losing meaningful digits.
func roundSignificant(value float64, digits int) float64 {
	if value == 0 {
		return 0
	}

	rounded, err := strconv.ParseFloat(strconv.FormatFloat(value, 'g', digits, 64), 64)
	if err != nil {
		return value
	}

	return rounded
}
