package tooluse

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	randomChoiceMaxItems     = 50
	randomChoiceMaxCount     = 10
	randomChoiceMaxItemRunes = 100
)

// randomIntN returns a uniform value in [0, n). The default source is the
// concurrency-safe auto-seeded global generator; tests inject their own.
func (r *Runtime) randomIntN(n int) int {
	if r.randIntN != nil {
		return r.randIntN(n)
	}

	return rand.IntN(n) //nolint:gosec // non-security choice among user-supplied options
}

func randomChoiceError(code, message, hint string) toolResult {
	return ErrorResult(NewToolError(code, ErrorArguments, message, true, hint))
}

func (r *Runtime) randomChoice(_ context.Context, callCtx CallContext, args json.RawMessage) (toolResult, error) {
	var payload struct {
		Items []string `json:"items"`
		Count *int     `json:"count"`
	}
	if !decodeToolJSON(args, &payload) {
		return toolResult{}, invalidArguments("Provide items (array of strings) and an optional count.")
	}
	count := 1
	if payload.Count != nil {
		count = *payload.Count
	}

	items := make([]string, len(payload.Items))
	if len(payload.Items) == 0 || len(payload.Items) > randomChoiceMaxItems {
		return randomChoiceError("invalid_items", fmt.Sprintf("items must contain 1 to %d entries.", randomChoiceMaxItems), "Provide between 1 and 50 options."), nil
	}
	for i, item := range payload.Items {
		item = strings.TrimSpace(item)
		switch {
		case item == "":
			return randomChoiceError("invalid_items", "items must not contain empty entries.", "Remove empty options."), nil
		case utf8.RuneCountInString(item) > randomChoiceMaxItemRunes:
			return randomChoiceError("invalid_items", fmt.Sprintf("each item must be at most %d characters.", randomChoiceMaxItemRunes), "Shorten long options."), nil
		case strings.ContainsFunc(item, unicode.IsControl):
			return randomChoiceError("invalid_items", "items must not contain control characters.", "Use plain text options."), nil
		}
		items[i] = item
	}
	if count < 1 || count > randomChoiceMaxCount {
		return randomChoiceError("invalid_count", fmt.Sprintf("count must be between 1 and %d.", randomChoiceMaxCount), "Request between 1 and 10 selections."), nil
	}
	if count > len(items) {
		return randomChoiceError("count_exceeds_items", fmt.Sprintf("count (%d) exceeds the number of items (%d).", count, len(items)), "Lower count or provide more items."), nil
	}
	if !randomChoiceDeliverable(items, count) {
		return randomChoiceError("selection_too_large", "The selected items could not be returned within the result budget.", "Shorten the items or lower count."), nil
	}

	// Partial Fisher-Yates over positions: without replacement, and
	// duplicate texts remain distinct entries.
	positions := make([]int, len(items))
	for i := range positions {
		positions[i] = i
	}
	selected := make([]map[string]any, 0, count)
	for i := 0; i < count; i++ {
		j := i + r.randomIntN(len(positions)-i)
		positions[i], positions[j] = positions[j], positions[i]
		selected = append(selected, map[string]any{"index": positions[i], "item": items[positions[i]]})
	}

	result := randomChoiceResult(selected, len(items))
	logToolResult(callCtx, result, "selected_count", count, "from_count", len(items))

	return result, nil
}

func randomChoiceResult(selected []map[string]any, fromCount int) toolResult {
	return toolResult{
		Status:  "ok",
		Summary: fmt.Sprintf("Selected %d item(s)", len(selected)),
		Data: map[string]any{
			"selected":   selected,
			"from_count": fromCount,
			"index_base": 0,
		},
	}
}

// randomChoiceDeliverable checks, before drawing, that the worst possible
// selection (the count items with the largest escaped JSON size, each at the
// widest index) still serializes within the budget without any reduction.
// Every accepted request can therefore return all its picks intact.
func randomChoiceDeliverable(items []string, count int) bool {
	sizes := make([]int, len(items))
	order := make([]int, len(items))
	for i, item := range items {
		encoded, err := json.Marshal(item)
		if err != nil {
			return false
		}
		sizes[i], order[i] = len(encoded), i
	}
	sort.SliceStable(order, func(a, b int) bool { return sizes[order[a]] > sizes[order[b]] })

	widest := len(items) - 1
	worst := make([]map[string]any, 0, count)
	for _, position := range order[:count] {
		worst = append(worst, map[string]any{"index": widest, "item": items[position]})
	}
	_, preparation := prepareResult(randomChoiceResult(worst, len(items)), randomChoiceResultCharBudget)

	return preparation == ResultPreparationComplete
}
