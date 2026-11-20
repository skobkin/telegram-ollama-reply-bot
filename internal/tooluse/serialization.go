package tooluse

import (
	"bytes"
	"encoding/json"
	"sort"
	"unicode/utf8"

	"telegram-ollama-reply-bot/internal/support/toolbudget"
)

// marshalResult reduces structured values, never an already encoded JSON string.
// Final escaped JSON characters (including omission metadata) count toward budget.
func marshalResult(result toolResult, budget int) string {
	if budget <= 0 {
		budget = defaultToolResultCharBudget
	}
	if budget < toolbudget.MinChars {
		budget = toolbudget.MinChars
	} // programmatic callers; ENV is validated at boot
	result = normalizeResult(result)
	encoded, err := json.Marshal(result)
	if err != nil {
		return resultDeliveryFailure(result, "result_serialization_failed")
	}
	if utf8.RuneCount(encoded) <= budget {
		return string(encoded)
	}
	var data any
	var tree map[string]json.RawMessage
	if json.Unmarshal(encoded, &tree) != nil {
		return resultDeliveryFailure(result, "result_serialization_failed")
	}
	if raw := tree["data"]; raw != nil {
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		if decoder.Decode(&data) != nil {
			return resultDeliveryFailure(result, "result_serialization_failed")
		}
	}
	result.Data = data
	result.Truncated = true
	result.Reductions = make(map[string]int)
	for {
		encoded, err = json.Marshal(result)
		if err != nil {
			return resultDeliveryFailure(result, "result_serialization_failed")
		}
		if utf8.RuneCount(encoded) <= budget {
			return string(encoded)
		}
		candidate := findReduction(result.Data, "data", false)
		if candidate.apply != nil {
			candidate.apply()
			// Bound the metadata too; truncated remains explicit if detailed paths
			// no longer fit. Never insert markers into caller-owned opaque objects.
			if len(result.Reductions) < 8 && len(candidate.path) <= 120 {
				result.Reductions[candidate.path] += candidate.removed
			}

			continue
		}
		if result.Summary != "" {
			result.Summary = ""

			continue
		}
		if result.Reductions != nil {
			result.Reductions = nil

			continue
		}

		return resultDeliveryFailure(result, "result_too_large")
	}
}

type reduction struct {
	size, removed int
	path          string
	apply         func()
}

func reducibleText(key string) bool {
	switch key {
	case "text", "summary", "description", "snippet", "analysis", "title", "content", "text_snippet", "explanation", "note":
		return true
	default:
		return false
	}
}

func semanticUnit(key string) bool {
	switch key {
	case "options", "probabilities", "levels", "selected", "error":
		return true
	default:
		return false
	}
}

// Unknown strings and all numeric/identity fields remain intact. Collections
// lose complete trailing items with explicit omission counts, never fragments.
func findReduction(value any, path string, protected bool) reduction {
	if protected {
		return reduction{}
	}
	best := reduction{}
	switch v := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(v))
		for key := range v {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			item := v[key]
			nextPath := path + "." + key
			// Bound reduction work for large external collections. Retain complete
			// items and halve before scanning potentially thousands of text fields.
			if list, ok := item.([]any); ok && len(list) > 32 && !semanticUnit(key) {
				keep := len(list) / 2

				return reduction{size: len(list) - keep, removed: len(list) - keep, path: nextPath, apply: func() { v[key] = list[:keep] }}
			}
			if text, ok := item.(string); ok && reducibleText(key) && utf8.RuneCountInString(text) > 32 {
				runes := []rune(text)
				keep := len(runes) / 2
				candidate := reduction{size: len(runes) - keep, removed: len(runes) - keep - 1, path: nextPath, apply: func() { v[key] = string(runes[:keep]) + "…" }}
				if candidate.size > best.size {
					best = candidate
				}
			}
			candidate := findReduction(item, nextPath, semanticUnit(key))
			if candidate.size > best.size {
				best = candidate
			}
			if list, ok := item.([]any); ok && len(list) > 0 && !semanticUnit(key) {
				// Preserve every item's identifiers while reducing text first.
				if best.apply == nil {
					best = reduction{size: 1, removed: 1, path: nextPath, apply: func() { v[key] = list[:len(list)-1] }}
				}
			}
		}
	case []any:
		for _, item := range v {
			candidate := findReduction(item, path+"[]", false)
			if candidate.size > best.size {
				best = candidate
			}
		}
	}

	return best
}

func resultDeliveryFailure(result toolResult, code string) string {
	failure := ErrorResult(NewToolError(code, ErrorPermanent, "The tool's result could not be delivered within its JSON budget.", false, "Do not repeat an action that already completed; its execution status is preserved."))
	failure.ExecutionStatus = result.Status
	failure.Truncated = true
	// Contract fields are fixed, bounded text, independent of the bad payload.
	encoded, _ := json.Marshal(failure)

	return string(encoded)
}
