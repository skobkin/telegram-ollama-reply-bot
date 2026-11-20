package tooluse

import (
	"context"
	"encoding/json"
	"math"
	"strings"
	"testing"
)

func calcValue(t *testing.T, expression string) float64 {
	t.Helper()
	result := evaluateCalculation(expression)
	if result.Status != "ok" {
		t.Fatalf("%q: unexpected failure: %+v", expression, result.Error)
	}
	data := result.Data.(map[string]any)

	return data["value"].(float64)
}

func calcErrorCode(t *testing.T, expression string) string {
	t.Helper()
	result := evaluateCalculation(expression)
	if result.Status != "error" || result.Error == nil {
		t.Fatalf("%q: expected an error, got %+v", expression, result)
	}
	if result.Error.Class != ErrorArguments || !result.Error.Retryable {
		t.Fatalf("%q: unexpected class/retryable: %+v", expression, result.Error)
	}

	return result.Error.Code
}

func TestCalculateValues(t *testing.T) {
	cases := map[string]float64{
		"1 / 2":                      0.5,
		"7 / 2":                      3.5,
		"2 + 3 * 4":                  14,
		"(2 + 3) * 4":                20,
		"2 ^ 3 ^ 2":                  512,
		"2 ** 3 ** 2":                512,
		"-2 ^ 2":                     -4,
		"(-2) ^ 2":                   4,
		"sqrt(16) + abs(-3)":         7,
		"max(1, 5, 3)":               5,
		"min(4, 2)":                  2,
		"pow(2, 10)":                 1024,
		"mod(10, 3)":                 1,
		"floor(2.7) + ceil(2.1)":     5,
		"round(2.5)":                 3,
		"hypot(3, 4)":                5,
		"log10(1000)":                3,
		"log2(8)":                    3,
		"ln(e)":                      1,
		"cos(0)":                     1,
		"pi":                         math.Pi,
		"99999999999 * 99999999999":  99999999999.0 * 99999999999.0,
		"9007199254740993 * 1000000": 9007199254740993.0 * 1000000.0,
		"+5":                         5,
		"1.5e3":                      1500,
	}
	for expression, want := range cases {
		if got := calcValue(t, expression); math.Abs(got-want) > 1e-9*math.Max(1, math.Abs(want)) {
			t.Errorf("%q = %v, want %v", expression, got, want)
		}
	}
}

func TestCalculateRejections(t *testing.T) {
	cases := map[string]string{
		"":                             "invalid_expression",
		"1 +":                          "invalid_expression",
		"(1":                           "invalid_expression",
		`"abc"`:                        "unsupported_operation",
		"true":                         "unsupported_operation",
		"nil":                          "unsupported_operation",
		"foo":                          "unsupported_operation",
		"foo(1)":                       "unsupported_operation",
		"len([1])":                     "unsupported_operation",
		"map([1], # + 1)":              "unsupported_operation",
		"[1,2][0]":                     "unsupported_operation",
		"1..3":                         "unsupported_operation",
		"1 > 0 ? 1 : 2":                "unsupported_operation",
		"1 < 2":                        "unsupported_operation",
		"7 % 2":                        "unsupported_operation",
		"1 and 2":                      "unsupported_operation",
		"!1":                           "unsupported_operation",
		"pi.x":                         "unsupported_operation",
		"sqrt()":                       "wrong_arity",
		"sqrt(1, 2)":                   "wrong_arity",
		"min(1)":                       "wrong_arity",
		"max(1, 2, 3, 4, 5)":           "wrong_arity",
		"atan2(1)":                     "wrong_arity",
		"1 / 0":                        "non_finite_result",
		"0 / 0":                        "non_finite_result",
		"sqrt(-1)":                     "non_finite_result",
		"ln(0)":                        "non_finite_result",
		"asin(2)":                      "non_finite_result",
		"10 ^ 400":                     "non_finite_result",
		"mod(5, 0)":                    "non_finite_result",
		"exp(1000)":                    "non_finite_result",
		strings.Repeat("1+", 40) + "1": "expression_too_complex",
		strings.Repeat("-", 40) + "1":  "expression_too_complex",
		strings.Repeat("1", 300):       "expression_too_complex",
	}
	for expression, want := range cases {
		if got := calcErrorCode(t, expression); got != want {
			t.Errorf("%q: code %q, want %q", expression, got, want)
		}
	}
}

func TestCalculateResultSizeIsIndependentOfInput(t *testing.T) {
	expression := strings.Repeat("1+", 100) // valid length, rejected for size; stays small
	text := marshalResult(evaluateCalculation(expression), calculateResultCharBudget)
	if len([]rune(text)) > calculateResultCharBudget {
		t.Fatalf("result exceeds budget: %d", len([]rune(text)))
	}

	// Escape-heavy 256-rune input must not leak into a valid result either.
	ok := evaluateCalculation("1 + 1")
	if strings.Contains(marshalResult(ok, calculateResultCharBudget), "expression") {
		t.Fatal("successful result must not echo the expression")
	}
	if _, prep := prepareResult(ok, calculateResultCharBudget); prep != ResultPreparationComplete {
		t.Fatalf("preparation = %s", prep)
	}
	hostile := evaluateCalculation(strings.Repeat("<\"\\", 100))
	if _, prep := prepareResult(hostile, calculateResultCharBudget); prep == ResultPreparationFailed {
		t.Fatal("hostile expression error failed delivery")
	}
}

func TestCalculateHandlerRejectsBadArguments(t *testing.T) {
	for _, raw := range []string{`{}`, `{"expression":1}`, `{"expression":"1","x":2}`, `[]`} {
		if raw == `{}` {
			result, err := calculateHandler(context.Background(), CallContext{}, json.RawMessage(raw))
			if err != nil || result.Error == nil || result.Error.Code != "invalid_expression" {
				t.Errorf("%s: %+v %v", raw, result, err)
			}

			continue
		}
		if _, err := calculateHandler(context.Background(), CallContext{}, json.RawMessage(raw)); err == nil {
			t.Errorf("%s: expected invalid arguments error", raw)
		}
	}
}
