package statistics

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
)

func populatedToolCollector(t *testing.T) *Collector {
	t.Helper()
	c := newToolCollector()
	c.SetToolCatalog([]string{"alpha", "beta", "gamma"})
	for chat := int64(1); chat <= MaxTrackedChats+3; chat++ {
		recordTurn(c, []string{"alpha", "beta"}, []ToolCallObservation{
			okCall("alpha"), errCall("beta", fmt.Sprintf("code_%d", chat%20), "transient"), okCall("beta"), emptyCall("alpha"),
		}, inChat(chat), withModel(fmt.Sprintf("model-%d", chat%11)))
	}
	recordTurn(c, []string{"alpha"}, []ToolCallObservation{rejectedCall("ghost", ToolRejectedNotRegistered), rejectedCall("beta", ToolRejectedNotAdvertised)}, proactive(), failing(ToolRunTimeout))
	effect := okCall("alpha")
	effect.SideEffecting, effect.SideEffectOutcome, effect.Preparation = true, "confirmed", ToolPreparationReduced
	recordTurn(c, []string{"alpha"}, []ToolCallObservation{effect}, withModel(strings.Repeat("x", maxModelIDBytes+1)))

	return c
}

func toolSnapshotNoPersistence(t *testing.T, c *Collector, scope InteractionScope) string {
	t.Helper()
	snap := c.ToolSnapshot(scope)
	snap.Persistence = PersistenceInfo{}
	data, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}

	return string(data)
}

func TestToolRoundTripPreservesSnapshotsExactly(t *testing.T) {
	src := populatedToolCollector(t)
	pair, payload := capturePayloadOf(t, src.ToolDataset())

	dst := newToolCollector()
	dst.SetToolCatalog([]string{"alpha", "beta", "gamma"})
	if err := dst.ToolDataset().Restore(toolFormatVersion, pair, payload); err != nil {
		t.Fatalf("restore: %v", err)
	}

	for _, scope := range []InteractionScope{GlobalScope(), ChatScope(1), ChatScope(7), ChatScope(MaxTrackedChats + 2)} {
		if want, got := toolSnapshotNoPersistence(t, src, scope), toolSnapshotNoPersistence(t, dst, scope); want != got {
			t.Fatalf("scope %+v differs after the round trip", scope)
		}
	}
	if dst.toolPersist.current != pair || dst.toolPersist.dirty() {
		t.Fatalf("the restored tracker must be clean at the persisted pair: %+v", dst.toolPersist)
	}
	if dst.tool.oversizedIDFolds != src.tool.oversizedIDFolds || src.tool.oversizedIDFolds == 0 {
		t.Fatal("fold counters are restored verbatim and restoration never recounts them")
	}
}

func TestToolRestoreKeepsAdmissionSoLaterFoldingStaysConsistent(t *testing.T) {
	src := newToolCollector()
	for i := 0; i < MaxErrorCodesPerTool+3; i++ {
		recordTurn(src, []string{"t"}, []ToolCallObservation{errCall("t", fmt.Sprintf("code_%02d", i), "permanent")}, inChat(1), withModel("m"))
	}
	pair, payload := capturePayloadOf(t, src.ToolDataset())
	dst := newToolCollector()
	if err := dst.ToolDataset().Restore(toolFormatVersion, pair, payload); err != nil {
		t.Fatal(err)
	}

	// A code that was folded before the restart must stay folded after it,
	// in every projection, even when another chat/model brings it now.
	recordTurn(dst, []string{"t"}, []ToolCallObservation{errCall("t", fmt.Sprintf("code_%02d", MaxErrorCodesPerTool+1), "permanent")}, inChat(2), withModel("other"))
	global := errorCodeCounts(findTool(t, dst.ToolSnapshot(GlobalScope()), "t").ErrorCodes)
	if global[overflowCodeLabel] != 4 || global[fmt.Sprintf("code_%02d", MaxErrorCodesPerTool+1)] != 0 {
		t.Fatalf("admission must survive the restart: %v", global)
	}
	if _, payload2 := capturePayloadOf(t, dst.ToolDataset()); newToolCollector().ToolDataset().Restore(toolFormatVersion, PersistPair{}, payload2) != nil {
		t.Fatal("the continued dataset must stay valid")
	}
}

func TestToolRestoreAppliesTheCatalogAndStaleToolsKeepTheirData(t *testing.T) {
	src := newToolCollector()
	recordTurn(src, []string{"old_tool"}, []ToolCallObservation{okCall("old_tool")})
	pair, payload := capturePayloadOf(t, src.ToolDataset())

	dst := newToolCollector()
	dst.SetToolCatalog([]string{"new_tool"}) // old_tool is no longer registered
	if err := dst.ToolDataset().Restore(toolFormatVersion, pair, payload); err != nil {
		t.Fatal(err)
	}
	snap := dst.ToolSnapshot(GlobalScope())
	old := findTool(t, snap, "old_tool")
	if old.Registered || old.Counters.Attempts != 1 {
		t.Fatalf("a no-longer-registered tool keeps its persisted data: %+v", old)
	}
	if fresh := findTool(t, snap, "new_tool"); !fresh.Registered || !fresh.NeverAdvertised {
		t.Fatalf("a newly registered tool appears as a zero-use row: %+v", fresh)
	}
}

func TestToolRestoreRejectsMalformedPayloads(t *testing.T) {
	src := populatedToolCollector(t)
	pair, payload := capturePayloadOf(t, src.ToolDataset())

	mutate := func(f func(p *toolPayload)) []byte {
		var p toolPayload
		if err := json.Unmarshal(payload, &p); err != nil {
			t.Fatal(err)
		}
		f(&p)
		out, err := json.Marshal(p)
		if err != nil {
			t.Fatal(err)
		}

		return out
	}
	cases := map[string]func(p *toolPayload){
		"succeeded + failed differs from executed": func(p *toolPayload) { p.ByModel[0].C[tcSucceeded]++ },
		"attempts differ from executed + rejected": func(p *toolPayload) { p.ByModel[0].C[tcAttempts]++ },
		"advertised-invoked exceeds advertised":    func(p *toolPayload) { p.ByModel[0].C[tcAdvertisedInvokedTurns] = p.ByModel[0].C[tcAdvertisedTurns] + 1 },
		"latency total differs from executed":      func(p *toolPayload) { p.ByModel[0].Latency[0]++ },
		"error classes differ from failures":       func(p *toolPayload) { p.ByModel[0].Classes[0]++ },
		"model and chat projections disagree": func(p *toolPayload) {
			p.ByChat[0].C[tcAdvertisedTurns]++
		},
		"tool dictionary over the cap": func(p *toolPayload) {
			for i := 0; i <= MaxTrackedTools; i++ {
				p.Tools = append(p.Tools, fmt.Sprintf("extra_%d", i))
			}
		},
		"error code dictionary over the cap": func(p *toolPayload) {
			for i := 0; i <= MaxTrackedErrorCodes; i++ {
				p.ErrorCodes = append(p.ErrorCodes, fmt.Sprintf("extra_%d", i))
			}
		},
		"invalid code in dictionary": func(p *toolPayload) { p.ErrorCodes = append(p.ErrorCodes, "NOT VALID") },
		"admission set over the per-tool cap": func(p *toolPayload) {
			p.Admission[0].Codes = make([]int, MaxErrorCodesPerTool+1)
			for i := range p.Admission[0].Codes {
				p.Admission[0].Codes[i] = i % len(p.ErrorCodes)
			}
		},
		"code not admitted for the tool": func(p *toolPayload) { p.Admission[0].Codes = nil },
		"dangling tool reference":        func(p *toolPayload) { p.ByModel[0].Tool = len(p.Tools) + 3 },
		"dangling chat reference":        func(p *toolPayload) { p.ByChat[0].Chat = len(p.Chats) + 3 },
		"mode out of range":              func(p *toolPayload) { p.ByModel[0].Mode = 9 },
		"duplicate by-model row":         func(p *toolPayload) { p.ByModel = append(p.ByModel, p.ByModel[0]) },
		"latency bin count mismatch":     func(p *toolPayload) { p.ByModel[0].Latency = p.ByModel[0].Latency[:2] },
		"turn counters disagree":         func(p *toolPayload) { p.TurnsModel[0].Turns++ },
		"turn with calls exceeds turns": func(p *toolPayload) {
			p.TurnsModel[0].WithCalls = p.TurnsModel[0].Turns + 1
			p.TurnsChat[0].WithCalls = p.TurnsChat[0].Turns + 1
		},
	}
	for name, f := range cases {
		t.Run(name, func(t *testing.T) {
			dst := newToolCollector()
			err := dst.ToolDataset().Restore(toolFormatVersion, pair, mutate(f))
			if !errors.Is(err, ErrMalformedSnapshot) {
				t.Fatalf("expected ErrMalformedSnapshot, got %v", err)
			}
			if len(dst.tool.byModel) != 0 {
				t.Fatal("a rejected restore must not install partial state")
			}
		})
	}
	for name, raw := range map[string][]byte{"not json": []byte("{"), "unknown field": []byte(`{"x":1}`), "empty": nil} {
		t.Run(name, func(t *testing.T) {
			if err := newToolCollector().ToolDataset().Restore(toolFormatVersion, pair, raw); !errors.Is(err, ErrMalformedSnapshot) {
				t.Fatalf("got %v", err)
			}
		})
	}
	if err := newToolCollector().ToolDataset().Restore(7, pair, payload); !errors.Is(err, ErrMalformedSnapshot) {
		t.Fatalf("an unsupported version must be malformed, got %v", err)
	}
}

func TestToolPersisterIntegrationResetDurabilityAndStaleWrites(t *testing.T) {
	store, c := newFakeStore(), populatedToolCollector(t)
	p := testPersister(store, nil, c.InteractionDataset(), c.ToolDataset())
	ctx := context.Background()

	if err := p.FlushNow(ctx, ToolDatasetName); err != nil {
		t.Fatal(err)
	}
	stale, _ := store.row(ToolDatasetName)
	if _, ok := store.row(InteractionDatasetName); ok {
		t.Fatal("datasets persist independently: nothing recorded in interaction")
	}

	generation := c.ResetToolUsage()
	if out := p.EnsureDurable(ctx, ToolDatasetName, generation); !out.Durable {
		t.Fatalf("%+v", out)
	}
	if applied, err := store.SaveSnapshot(ctx, stale); err != nil || applied {
		t.Fatalf("a delayed pre-reset write must be rejected: applied=%v err=%v", applied, err)
	}

	restarted := newToolCollector()
	if err := testPersister(store, nil, restarted.ToolDataset()).Restore(ctx); err != nil {
		t.Fatal(err)
	}
	snap := restarted.ToolSnapshot(GlobalScope())
	if snap.Period.Resets != 1 || snap.Turns[0].Turns != 0 {
		t.Fatalf("a restart shows the reset state: %+v", snap.Period)
	}
}

func TestToolResetIsDirtyEvenWhenRevisionEqualsSavedRevision(t *testing.T) {
	store, c := newFakeStore(), newToolCollector()
	p := testPersister(store, nil, c.ToolDataset())
	recordTurn(c, []string{"t"}, nil)
	if err := p.FlushNow(context.Background(), ToolDatasetName); err != nil {
		t.Fatal(err)
	}
	saved := c.toolPersist.saved.Revision
	c.ResetToolUsage()
	recordTurn(c, []string{"t"}, nil)
	if c.toolPersist.current.Revision != saved || !c.toolPersist.dirty() {
		t.Fatalf("equal revision in a newer generation must be dirty: %+v / %+v", c.toolPersist.current, c.toolPersist.saved)
	}
}

func TestToolJSONExportIsVersionedAndPrivacyClean(t *testing.T) {
	c := populatedToolCollector(t)
	snap := c.ToolSnapshot(GlobalScope())
	data, err := ToolJSON(snap, GlobalScope())
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"schema", "version", "generated_at", "scope", "observation_period", "persistence", "turns", "totals", "tools", "models", "catalog", "per_chat_complete", "model_attribution_complete", "oversized_id_folds", "notes"} {
		if _, ok := raw[key]; !ok {
			t.Fatalf("export is missing %q", key)
		}
	}
	var schema string
	_ = json.Unmarshal(raw["schema"], &schema)
	if schema != ToolSchemaName {
		t.Fatalf("schema %q", schema)
	}
	text := string(data)
	for _, forbidden := range []string{"ghost", `"arguments"`, "call_id", "run_id", "prompt", "user_id", "NaN", "Inf"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("the export must not contain %q", forbidden)
		}
	}
	// Chat scope documents itself and has no model attribution.
	chat, err := ToolJSON(c.ToolSnapshot(ChatScope(3)), ChatScope(3))
	if err != nil || !strings.Contains(string(chat), `"kind": "chat"`) {
		t.Fatalf("chat scope export: %v", err)
	}
}

// TestToolWorstCasePayloadFitsTheWriteLimit saturates every dimension of the
// tool dataset (all caps plus overflow keys) with maximum-width counters.
func TestToolWorstCasePayloadFitsTheWriteLimit(t *testing.T) {
	c := newToolCollector()
	tools := make([]string, 0, MaxTrackedTools+2)
	for i := 0; i < MaxTrackedTools+2; i++ {
		tools = append(tools, fmt.Sprintf("tool_%03d", i))
	}
	codes := make([]string, MaxTrackedErrorCodes)
	for i := range codes {
		codes[i] = fmt.Sprintf("code_%02d", i)
	}
	for chat := int64(1); chat <= MaxTrackedChats+2; chat++ {
		for m := 0; m < MaxTrackedModels+2; m++ {
			for mode := ToolMode(0); mode < toolModeCount; mode++ {
				var calls []ToolCallObservation
				for i, name := range tools {
					calls = append(calls, errCall(name, codes[(i+int(chat))%len(codes)], "permanent"))
				}
				calls = append(calls, rejectedCall("ghost", ToolRejectedNotRegistered))
				recordTurn(c, tools, calls, inChat(chat), withModel(fmt.Sprintf("model-%d-%s", m, strings.Repeat("x", maxModelIDBytes-12))), func(o *ToolTurnObservation) { o.Mode = mode })
			}
		}
	}

	full := uint64(math.MaxUint64)
	saturate := func(r *toolRow) {
		for i := range r.c {
			r.c[i] = full
		}
		for i := range r.classes {
			r.classes[i] = full
		}
		for id := range r.codes {
			r.codes[id] = full
		}
		saturateHistogram(&r.latency)
	}
	for _, r := range c.tool.byModel {
		saturate(r)
	}
	for _, r := range c.tool.byChat {
		saturate(r)
	}

	bound := toolWorstCase()
	if got := len(c.tool.byModel); got != bound.modelRows {
		t.Fatalf("by-model rows = %d, derived worst case %d", got, bound.modelRows)
	}
	if got := len(c.tool.byChat); got != bound.chatRows {
		t.Fatalf("by-chat rows = %d, derived worst case %d", got, bound.chatRows)
	}
	_, payload := capturePayloadOf(t, c.ToolDataset())
	t.Logf("tool worst case: %d + %d rows, payload %.1f MB, derived bound %.1f MB, counters in RAM %.1f MB",
		bound.modelRows, bound.chatRows, float64(len(payload))/(1<<20), float64(bound.payloadBytes)/(1<<20), float64(bound.counterBytesRAM)/(1<<20))
	if len(payload) > bound.payloadBytes {
		t.Fatalf("payload %d bytes exceeds the derived bound %d", len(payload), bound.payloadBytes)
	}
	if bound.payloadBytes >= MaxSnapshotPayloadBytes {
		t.Fatalf("derived bound %d does not fit the %d byte write limit", bound.payloadBytes, MaxSnapshotPayloadBytes)
	}
	_ = time.Second
}
