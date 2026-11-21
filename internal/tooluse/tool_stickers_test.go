package tooluse

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"telegram-ollama-reply-bot/internal/stickers"
)

type staticCatalog struct{ index *stickers.Index }

func (c staticCatalog) Snapshot() *stickers.Index { return c.index }

var stickerTestNow = time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)

func stickerEntry(n int, desc string, tags ...string) stickers.Entry {
	return stickers.Entry{
		SetName: "Kitsu", FileUniqueID: fmt.Sprintf("u%02d", n), FileID: fmt.Sprintf("SECRET-FILE-ID-%02d", n),
		SemanticID: fmt.Sprintf("sticker_%02d", n), Description: desc, Tags: tags, Emoji: "🦊",
		Format: stickers.FormatStatic, Position: n, Available: true, Enabled: true, SyncedAt: stickerTestNow, UpdatedAt: stickerTestNow,
	}
}

func stickerIndex(t *testing.T, title string, entries ...stickers.Entry) *stickers.Index {
	t.Helper()
	state := stickers.State{Sets: []stickers.Set{{Name: "Kitsu", Title: title, Enabled: true, Revision: 1}}, Entries: entries}
	if err := stickers.ValidateState(state); err != nil {
		t.Fatalf("fixture invalid: %v", err)
	}

	return stickers.BuildIndex(state)
}

func stickerRuntime(index *stickers.Index) *Runtime {
	r := &Runtime{registry: newRegistry(&Runtime{})}
	r.AttachStickerCatalog(staticCatalog{index})

	return r
}

func callListStickers(t *testing.T, r *Runtime, payload string) toolResult {
	t.Helper()
	result, err := r.listStickers(context.Background(), CallContext{}, json.RawMessage(payload))
	if err != nil {
		t.Fatalf("listStickers: %v", err)
	}

	return result
}

func listed(t *testing.T, result toolResult) []stickerListItem {
	t.Helper()
	if result.Status != "ok" {
		t.Fatalf("unexpected failure: %+v", result.Error)
	}

	return result.Data.(map[string]any)["stickers"].([]stickerListItem)
}

func TestAttachStickerCatalogRegistersListStickersOnlyForAUsableCatalog(t *testing.T) {
	r := &Runtime{registry: newRegistry(&Runtime{})}
	r.AttachStickerCatalog(nil)
	if r.HasTool(listStickersToolName) {
		t.Fatalf("a nil catalog must not register the tool")
	}

	r = stickerRuntime(stickerIndex(t, "Kitsu", stickerEntry(1, "Happy fox")))
	if !r.HasTool(listStickersToolName) {
		t.Fatalf("an attached catalog registers the tool")
	}
	definition, _ := r.registry.Lookup(listStickersToolName)
	if definition.SideEffecting || definition.InvocationPolicy != InvocationPolicyDiscretionary {
		t.Fatalf("list_stickers is a discretionary read-only tool: %+v", definition)
	}
	if _, ok := definition.Resolve(CallContext{}, Presentation{}); !ok {
		t.Fatalf("a catalog with active stickers must be advertised")
	}
	for _, want := range []string{"mostly in English", "translate", "tags may be in other languages"} {
		if !strings.Contains(definition.Description, want) {
			t.Fatalf("the description must guide multilingual search (missing %q): %s", want, definition.Description)
		}
	}

	empty := stickerRuntime(stickers.BuildIndex(stickers.State{}))
	definition, _ = empty.registry.Lookup(listStickersToolName)
	if _, ok := definition.Resolve(CallContext{}, Presentation{}); ok {
		t.Fatalf("a catalog without active stickers must not be advertised")
	}
}

func TestListStickersReturnsCuratedPresentationOnly(t *testing.T) {
	r := stickerRuntime(stickerIndex(t, "Kitsu pack",
		stickerEntry(1, "Happy fox waving hello", "happy", "greeting"),
		stickerEntry(2, "Sad fox crying", "sad"),
	))

	result := callListStickers(t, r, `{"query":"hello"}`)
	items := listed(t, result)
	if len(items) != 1 || items[0].ID != "sticker_01" || items[0].Description != "Happy fox waving hello" || items[0].Set != "Kitsu pack" || items[0].Emoji != "🦊" {
		t.Fatalf("unexpected result: %+v", items)
	}

	wire, preparation := prepareResult(result, listStickersResultCharBudget)
	if preparation != ResultPreparationComplete {
		t.Fatalf("a small result must serialize completely, got %s", preparation)
	}
	for _, leaked := range []string{"SECRET-FILE-ID", "file_id", "file_unique_id", "u01"} {
		if strings.Contains(wire, leaked) {
			t.Fatalf("the result must not expose %q: %s", leaked, wire)
		}
	}
	data := result.Data.(map[string]any)
	if data["total_matches"] != 1 || data["returned"] != 1 || result.Truncated {
		t.Fatalf("unexpected counters: %+v truncated=%v", data, result.Truncated)
	}
}

func TestListStickersEmptyQueryAndLimit(t *testing.T) {
	r := stickerRuntime(stickerIndex(t, "Kitsu", stickerEntry(1, "One"), stickerEntry(2, "Two"), stickerEntry(3, "Three")))

	items := listed(t, callListStickers(t, r, `{}`))
	if len(items) != 3 || items[0].ID != "sticker_01" || items[2].ID != "sticker_03" {
		t.Fatalf("an empty query lists in stable order: %+v", items)
	}
	result := callListStickers(t, r, `{"limit":2}`)
	if got := listed(t, result); len(got) != 2 || result.Data.(map[string]any)["total_matches"] != 3 {
		t.Fatalf("limit must cap the rows but report the total: %+v", result.Data)
	}
	if result.Truncated {
		t.Fatalf("a model-requested limit is not a budget truncation")
	}
	if got := listed(t, callListStickers(t, r, `{"query":"nonexistent"}`)); got == nil || len(got) != 0 {
		t.Fatalf("no match must be an empty list, got %#v", got)
	}
	wire, _ := prepareResult(callListStickers(t, r, `{"query":"nonexistent"}`), listStickersResultCharBudget)
	if !strings.Contains(wire, `"stickers":[]`) {
		t.Fatalf("an empty result must serialize as an empty array: %s", wire)
	}
}

func TestListStickersTagFilter(t *testing.T) {
	r := stickerRuntime(stickerIndex(t, "Kitsu", stickerEntry(1, "One", "happy"), stickerEntry(2, "Two", "sad")))
	items := listed(t, callListStickers(t, r, `{"tags":["SAD"]}`))
	if len(items) != 1 || items[0].ID != "sticker_02" {
		t.Fatalf("tag filter: %+v", items)
	}
}

func TestListStickersArgumentValidation(t *testing.T) {
	r := stickerRuntime(stickerIndex(t, "Kitsu", stickerEntry(1, "One")))

	for name, payload := range map[string]string{
		"long query": fmt.Sprintf(`{"query":%q}`, strings.Repeat("я", stickers.MaxSearchQueryRunes+1)),
		"many tags":  `{"tags":["a","b","c","d","e","f","g","h","i"]}`,
		"empty tag":  `{"tags":["  "]}`,
		"long tag":   fmt.Sprintf(`{"tags":[%q]}`, strings.Repeat("т", stickers.MaxTagRunes+1)),
		"zero limit": `{"limit":0}`,
		"big limit":  `{"limit":21}`,
	} {
		result := callListStickers(t, r, payload)
		if result.Status != "error" || result.Error == nil || !result.Error.Retryable || result.Error.Class != ErrorArguments {
			t.Errorf("%s must be a retryable argument error: %+v", name, result)
		}
	}
	for name, payload := range map[string]string{"unknown field": `{"file_id":"x"}`, "wrong type": `{"limit":"ten"}`, "not an object": `[1]`} {
		if _, err := r.listStickers(context.Background(), CallContext{}, json.RawMessage(payload)); err == nil {
			t.Errorf("%s must be rejected as invalid arguments", name)
		}
	}
	if _, err := r.listStickers(context.Background(), CallContext{}, nil); err != nil {
		t.Errorf("absent arguments are an empty query: %v", err)
	}
}

func worstCaseEntries(n int, runeSet string) []stickers.Entry {
	repeat := func(count int) string {
		var b strings.Builder
		for i := 0; b.Len() < count*4 && utf8.RuneCountInString(b.String()) < count; i++ {
			b.WriteString(runeSet)
		}

		return string([]rune(b.String())[:count])
	}
	entries := make([]stickers.Entry, 0, n)
	for i := 1; i <= n; i++ {
		tags := make([]string, stickers.MaxTags)
		for j := range tags {
			tags[j] = fmt.Sprintf("%02d", j) + string([]rune(repeat(stickers.MaxTagRunes-2)))
		}
		entries = append(entries, stickerEntry(i, repeat(stickers.MaxDescriptionRunes), tags...))
	}

	return entries
}

func stickerRank(item stickerListItem) int {
	switch {
	case item.Set != "":
		return 3 // full
	case item.Description != "":
		return 2 // reduced
	default:
		return 1 // id only
	}
}

func TestListStickersEscapeHeavyWorstCaseStaysWithinBudgetWithoutRuntimeReduction(t *testing.T) {
	for name, runeSet := range map[string]string{
		"html escapes":    `<>&`,
		"quote and slash": `"\`,
		"multibyte":       `я🦊€`,
	} {
		t.Run(name, func(t *testing.T) {
			title := strings.Repeat(string([]rune(runeSet)[:1]), stickers.MaxTitleRunes)
			r := stickerRuntime(stickerIndex(t, title, worstCaseEntries(20, runeSet)...))

			result := callListStickers(t, r, `{"limit":20}`)
			items := listed(t, result)
			wire, preparation := prepareResult(result, listStickersResultCharBudget)
			if preparation != ResultPreparationComplete {
				t.Fatalf("the handler must fit the result itself, runtime preparation was %s", preparation)
			}
			if utf8.RuneCountInString(wire) > listStickersResultCharBudget {
				t.Fatalf("result is %d runes, budget %d", utf8.RuneCountInString(wire), listStickersResultCharBudget)
			}
			if len(items) == 0 || items[0].ID != "sticker_01" {
				t.Fatalf("the first match must always be returned with its exact id: %+v", items)
			}

			data := result.Data.(map[string]any)
			omitted, _ := data["omitted"].(int)
			t.Logf("returned=%d omitted=%d reduced=%v wire=%d runes", len(items), omitted, data["presentation_reduced"], utf8.RuneCountInString(wire))
			if len(items)+omitted != 20 || data["returned"] != len(items) || data["total_matches"] != 20 {
				t.Fatalf("counters must add up: returned=%d omitted=%d data=%+v", len(items), omitted, data)
			}
			if !result.Truncated {
				t.Fatalf("budget pressure must be reported")
			}
			reducedCount := 0
			for i, item := range items {
				if want := fmt.Sprintf("sticker_%02d", i+1); item.ID != want {
					t.Fatalf("ids are never altered and stay in rank order: %q at %d", item.ID, i)
				}
				if i > 0 && stickerRank(item) > stickerRank(items[i-1]) {
					t.Fatalf("with uniform entries the tiers may only degrade: %+v", items)
				}
				if stickerRank(item) < 3 {
					reducedCount++
				}
			}
			if got, _ := data["presentation_reduced"].(int); got != reducedCount {
				t.Fatalf("presentation_reduced=%d but %d rows were reduced", got, reducedCount)
			}
		})
	}
}

func TestListStickersOversizedAnnotationDoesNotDiscardOtherMatches(t *testing.T) {
	worst := worstCaseEntries(1, `<>&`)[0]
	entries := []stickers.Entry{worst}
	for i := 2; i <= 6; i++ {
		entries = append(entries, stickerEntry(i, "Tiny"))
	}
	r := stickerRuntime(stickerIndex(t, "Kitsu", entries...))

	result := callListStickers(t, r, `{}`)
	items := listed(t, result)
	if len(items) != 6 {
		t.Fatalf("an oversized first annotation must not push out the other matches: got %d of 6", len(items))
	}
	if stickerRank(items[0]) == 3 {
		t.Fatalf("the oversized first row must have been reduced, got a full row")
	}
	for _, item := range items[1:] {
		if stickerRank(item) != 3 {
			t.Fatalf("small rows after it keep their full presentation: %+v", item)
		}
	}
	if _, preparation := prepareResult(result, listStickersResultCharBudget); preparation != ResultPreparationComplete {
		t.Fatalf("runtime preparation must be complete, got %s", preparation)
	}
}

func TestListStickersTiersAreCappedIndependentOfStoredLength(t *testing.T) {
	long := strings.Repeat("слово ", 40)
	r := stickerRuntime(stickerIndex(t, strings.Repeat("T", 100), stickerEntry(1, string([]rune(long)[:240]), "a")))

	item := listed(t, callListStickers(t, r, `{}`))[0]
	if utf8.RuneCountInString(item.Description) != stickerFullDescriptionRunes || !strings.HasSuffix(item.Description, "…") {
		t.Fatalf("descriptions are capped at %d runes with a marker: %q", stickerFullDescriptionRunes, item.Description)
	}
	if utf8.RuneCountInString(item.Set) != stickerFullSetTitleRunes {
		t.Fatalf("set titles are capped at %d runes: %q", stickerFullSetTitleRunes, item.Set)
	}
}
