package tooluse

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
)

func callRandomChoice(t *testing.T, r *Runtime, payload string) (toolResult, error) {
	t.Helper()

	return r.randomChoice(context.Background(), CallContext{}, json.RawMessage(payload))
}

func selectedOf(t *testing.T, result toolResult) []map[string]any {
	t.Helper()
	if result.Status != "ok" {
		t.Fatalf("unexpected failure: %+v", result.Error)
	}

	return result.Data.(map[string]any)["selected"].([]map[string]any)
}

func TestRandomChoiceDrawsDistinctZeroBasedPositions(t *testing.T) {
	r := &Runtime{randIntN: func(int) int { return 0 }} // always the first remaining
	result, err := callRandomChoice(t, r, `{"items":["a","b","c","d"],"count":3}`)
	if err != nil {
		t.Fatal(err)
	}
	selected := selectedOf(t, result)
	if len(selected) != 3 {
		t.Fatalf("selected %d", len(selected))
	}
	for i, want := range []string{"a", "b", "c"} {
		if selected[i]["item"] != want || selected[i]["index"] != i {
			t.Errorf("selected[%d] = %v", i, selected[i])
		}
	}
	if result.Data.(map[string]any)["index_base"] != 0 {
		t.Error("index base must be reported as zero")
	}
}

func TestRandomChoiceDuplicateTextsStayDistinctAndNoRepeats(t *testing.T) {
	r := &Runtime{}
	for n := 0; n < 200; n++ {
		result, _ := callRandomChoice(t, r, `{"items":["x","x","y"],"count":3}`)
		seen := map[any]bool{}
		for _, entry := range selectedOf(t, result) {
			if seen[entry["index"]] {
				t.Fatalf("index repeated: %v", entry)
			}
			seen[entry["index"]] = true
		}
	}
}

func TestRandomChoiceDefaultCountAndTrimming(t *testing.T) {
	r := &Runtime{randIntN: func(int) int { return 0 }}
	result, _ := callRandomChoice(t, r, `{"items":["  pizza  ","sushi"]}`)
	selected := selectedOf(t, result)
	if len(selected) != 1 || selected[0]["item"] != "pizza" {
		t.Fatalf("selected = %v", selected)
	}
}

func TestRandomChoiceValidation(t *testing.T) {
	long := strings.Repeat("я", 101)
	cases := map[string]string{
		`{}`:                                  "invalid_items",
		`{"items":[]}`:                        "invalid_items",
		`{"items":["a",""]}`:                  "invalid_items",
		`{"items":["a","   "]}`:               "invalid_items",
		`{"items":["a\u0000b"]}`:              "invalid_items",
		`{"items":["a\nb"]}`:                  "invalid_items",
		fmt.Sprintf(`{"items":["%s"]}`, long): "invalid_items",
		`{"items":["a","b"],"count":0}`:       "invalid_count",
		`{"items":["a","b"],"count":11}`:      "invalid_count",
		`{"items":["a","b"],"count":3}`:       "count_exceeds_items",
		manyItems(51):                         "invalid_items",
	}
	r := &Runtime{}
	for payload, code := range cases {
		result, err := callRandomChoice(t, r, payload)
		if err != nil || result.Error == nil || result.Error.Code != code || result.Error.Class != ErrorArguments || !result.Error.Retryable {
			t.Errorf("%.60s: got %+v err=%v, want %s", payload, result.Error, err, code)
		}
	}
	for _, bad := range []string{`{"items":"a"}`, `{"items":["a"],"extra":1}`, `{"items":["a"],"count":"2"}`} {
		if _, err := callRandomChoice(t, r, bad); err == nil {
			t.Errorf("%s: expected invalid arguments", bad)
		}
	}
}

func manyItems(n int) string {
	items := make([]string, n)
	for i := range items {
		items[i] = fmt.Sprintf("%q", fmt.Sprintf("item%d", i))
	}

	return `{"items":[` + strings.Join(items, ",") + `]}`
}

func TestRandomChoiceRejectsUndeliverableSelection(t *testing.T) {
	// Ten items of 100 escape-heavy runes can never fit the budget.
	item := strings.Repeat("<", 100)
	items := make([]string, 10)
	for i := range items {
		items[i] = fmt.Sprintf("%q", item)
	}
	payload := `{"items":[` + strings.Join(items, ",") + `],"count":10}`
	result, err := callRandomChoice(t, &Runtime{}, payload)
	if err != nil || result.Error == nil || result.Error.Code != "selection_too_large" {
		t.Fatalf("got %+v err=%v", result.Error, err)
	}
}

func TestRandomChoiceAcceptedRequestsAlwaysDeliverIntact(t *testing.T) {
	payloads := []string{
		`{"items":["a","b","c"],"count":3}`,
		manyItems(50),
		fmt.Sprintf(`{"items":[%q,%q,%q],"count":2}`, strings.Repeat("я", 100), strings.Repeat("\"", 40), "short"),
	}
	// Ten maximum-length plain items fit exactly the budget class.
	plain := make([]string, 10)
	for i := range plain {
		plain[i] = fmt.Sprintf("%q", strings.Repeat("w", 100))
	}
	payloads = append(payloads, `{"items":[`+strings.Join(plain, ",")+`],"count":10}`)

	for _, payload := range payloads {
		for trial := 0; trial < 50; trial++ {
			var raw struct {
				Items []string `json:"items"`
			}
			if err := json.Unmarshal([]byte(payload), &raw); err != nil {
				t.Fatal(err)
			}
			result, err := callRandomChoice(t, &Runtime{}, payload)
			if err != nil {
				t.Fatal(err)
			}
			if result.Status != "ok" {
				if result.Error == nil || result.Error.Code != "selection_too_large" {
					t.Fatalf("unexpected error %+v", result.Error)
				}

				break
			}
			text, preparation := prepareResult(result, randomChoiceResultCharBudget)
			if preparation != ResultPreparationComplete {
				t.Fatalf("accepted selection not delivered intact (%s): %.80s", preparation, text)
			}
		}
	}
}

func TestRandomChoiceConcurrentUse(t *testing.T) {
	r := &Runtime{}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 0; n < 100; n++ {
				if result, _ := r.randomChoice(context.Background(), CallContext{}, json.RawMessage(`{"items":["a","b","c","d"],"count":2}`)); result.Status != "ok" {
					t.Error("failed")
				}
			}
		}()
	}
	wg.Wait()
}
