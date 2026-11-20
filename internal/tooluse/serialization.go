package tooluse

import (
	"bytes"
	"encoding/json"
	"sort"
	"strings"
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
		return resultDeliveryFailure(result, "result_serialization_failed", budget)
	}
	if utf8.RuneCount(encoded) <= budget {
		return string(encoded)
	}
	var data any
	var tree map[string]json.RawMessage
	if json.Unmarshal(encoded, &tree) != nil {
		return resultDeliveryFailure(result, "result_serialization_failed", budget)
	}
	if raw := tree["data"]; raw != nil {
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		if decoder.Decode(&data) != nil {
			return resultDeliveryFailure(result, "result_serialization_failed", budget)
		}
	}
	result.Data = data
	result.Truncated = true
	result.Reductions = make(map[string]int)
	for {
		encoded, err = json.Marshal(result)
		if err != nil {
			return resultDeliveryFailure(result, "result_serialization_failed", budget)
		}
		if utf8.RuneCount(encoded) <= budget {
			return string(encoded)
		}
		candidate := findReduction(result.Data, "data", false)
		if candidate.apply != nil {
			candidate.apply()
			if data, ok := result.Data.(map[string]any); ok {
				if flags, ok := data["truncated"].(map[string]any); ok {
					switch {
					case candidate.path == "data.chain":
						flags["chain_depth"] = true
					case candidate.path == "data.adjacent_replies" || (strings.HasPrefix(candidate.path, "data.adjacent_replies[]") && strings.HasSuffix(candidate.path, ".replies")):
						flags["adjacent_replies"] = true
					}
				}
			}
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

		return resultDeliveryFailure(result, "result_too_large", budget)
	}
}

type reduction struct {
	size, removed int
	path          string
	apply         func()
}

func reducibleText(key string) bool {
	switch key {
	case "text", "summary", "description", "snippet", "analysis", "title", "content", "text_snippet", "text_preview", "description_preview", "explanation", "note":
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
				candidate := reduction{size: len(runes) - keep, removed: len(runes) - keep - 1, path: nextPath, apply: func() {
					v[key] = string(runes[:keep]) + "…"
					if key == "text_preview" {
						if _, ok := v["text_truncated"]; ok {
							v["text_truncated"] = true
						}
					}
				}}
				if candidate.size > best.size {
					best = candidate
				}
			}
			candidate := findReduction(item, nextPath, semanticUnit(key))
			if candidate.size > best.size {
				best = candidate
			}
			if list, ok := item.([]any); ok && len(list) > 0 && !semanticUnit(key) {
				// Thread chains are oldest first. Retain the trigger and its immediate
				// reply target, and shed older ancestors only after adjacent replies.
				if nextPath == "data.chain" {
					if len(list) > 2 && best.apply == nil {
						best = reduction{size: 1, removed: 1, path: nextPath, apply: func() { v[key] = list[1:] }}
					} else if len(list) <= 2 && best.apply == nil {
						// If even the essential pair is too large, retain their identities
						// and reply links while omitting optional presentation details.
						for _, entry := range list {
							message, ok := entry.(map[string]any)
							if !ok {
								continue
							}
							count := len(message)
							if _, ok := message["message_id"]; ok {
								count--
							}
							if _, ok := message["reply_to_message_id"]; ok {
								count--
							}
							if count > 0 {
								best = reduction{size: 1, removed: count, path: nextPath + "[].details", apply: func() {
									for field := range message {
										if field != "message_id" && field != "reply_to_message_id" {
											delete(message, field)
										}
									}
								}}

								break
							}
						}
					}
				} else if best.apply == nil {
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

func resultDeliveryFailure(result toolResult, code string, budget int) string {
	if result.Error != nil {
		// Retain the original failure and retry semantics when optional data is
		// oversized or unserializable. Escaped text may still exceed the budget.
		result.Data = nil
		result.Summary = ""
		result.Reductions = nil
		result.Truncated = true
		for {
			encoded, err := json.Marshal(result)
			if err == nil && utf8.RuneCount(encoded) <= budget {
				return string(encoded)
			}
			e := *result.Error
			switch {
			case len([]rune(e.Message)) >= len([]rune(e.RecoveryHint)) && len([]rune(e.Message)) > 32:
				e.Message = truncateUTF8(e.Message, len([]rune(e.Message))/2)
			case len([]rune(e.RecoveryHint)) > 32:
				e.RecoveryHint = truncateUTF8(e.RecoveryHint, len([]rune(e.RecoveryHint))/2)
			default:
				return resultDeliveryFallback(result, code)
			}
			result.Error = &e
		}
	}

	return resultDeliveryFallback(result, code)
}

func resultDeliveryFallback(result toolResult, code string) string {
	failure := ErrorResult(NewToolError(code, ErrorPermanent, "The tool's result could not be delivered within its JSON budget.", false, "Do not repeat an action that already completed; its execution status is preserved."))
	failure.ExecutionStatus = result.Status
	failure.Truncated = true
	// Contract fields are fixed, bounded text, independent of the bad payload.
	encoded, _ := json.Marshal(failure)

	return string(encoded)
}
