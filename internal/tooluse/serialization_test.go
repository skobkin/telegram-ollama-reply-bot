package tooluse

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"telegram-ollama-reply-bot/internal/support/toolbudget"
)

func TestStructuredResultBudgetPreservesIdentitiesAndErrors(t *testing.T) {
	for _, budget := range []int{512, 600, 1800, 3500} {
		t.Run(strconv.Itoa(budget), func(t *testing.T) {
			rows := []map[string]any{}
			for i := 0; i < 30; i++ {
				rows = append(rows, map[string]any{"message_id": i + 1, "text": strings.Repeat("\"\\\n<&🙂", 200), "value": json.Number("9007199254740993")})
			}
			original := toolResult{Status: "ok", Data: map[string]any{"message_id": 123, "matches": rows}}
			wire := marshalResult(original, budget)
			if !json.Valid([]byte(wire)) || utf8.RuneCountInString(wire) > budget {
				t.Fatalf("invalid or oversized: %s", wire)
			}
			var decoded struct {
				Status    string
				Truncated bool
				Data      struct {
					MessageID int `json:"message_id"`
					Matches   []struct{ Value json.Number }
				}
			}
			if err := json.Unmarshal([]byte(wire), &decoded); err != nil {
				t.Fatal(err)
			}
			if decoded.Status != "ok" || !decoded.Truncated || decoded.Data.MessageID != 123 {
				t.Fatal(wire)
			}
			for _, row := range decoded.Data.Matches {
				if row.Value != "9007199254740993" {
					t.Fatal("numeric precision lost")
				}
			}
			if len(rows) != 30 || rows[0]["text"] != strings.Repeat("\"\\\n<&🙂", 200) {
				t.Fatal("handler-owned data mutated")
			}
		})
	}
	result := ErrorResult(NewToolError("invalid_emoji", ErrorArguments, "Choose an available emoji.", true, "Use a value from the current enum."))
	result.Data = map[string]any{"message_id": 99, "text": strings.Repeat("x", 9000)}
	wire := marshalResult(result, 512)
	var decoded ToolResult
	if err := json.Unmarshal([]byte(wire), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Error == nil || decoded.Error.Code != "invalid_emoji" || !decoded.Error.Retryable || decoded.Error.RecoveryHint == "" {
		t.Fatal(wire)
	}
}

func TestIrreducibleAndUnserializableResultsReportDeliveryFailure(t *testing.T) {
	for _, data := range []any{map[string]any{"id": strings.Repeat("a", 1000)}, make(chan int), json.Number("true")} {
		wire := marshalResult(toolResult{Status: "ok", Data: data}, toolbudget.MinChars)
		var result ToolResult
		if !json.Valid([]byte(wire)) || utf8.RuneCountInString(wire) > toolbudget.MinChars {
			t.Fatal(wire)
		}
		if err := json.Unmarshal([]byte(wire), &result); err != nil {
			t.Fatal(err)
		}
		if result.ExecutionStatus != "ok" || result.Error == nil || result.Error.Retryable {
			t.Fatal(wire)
		}
	}
}
