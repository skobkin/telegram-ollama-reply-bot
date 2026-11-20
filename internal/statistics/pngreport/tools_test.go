package pngreport

import (
	"bytes"
	"fmt"
	"image/png"
	"testing"
	"time"

	"telegram-ollama-reply-bot/internal/statistics"
)

func toolCollectorWith(t *testing.T, names []string, used int) *statistics.Collector {
	t.Helper()
	c := statistics.NewCollector(statistics.Config{})
	c.SetToolCatalog(names)
	var calls []statistics.ToolCallObservation
	for i := 0; i < used && i < len(names); i++ {
		calls = append(calls, statistics.ToolCallObservation{Name: names[i], Executed: true, Status: "ok", Duration: time.Duration(10+i) * time.Millisecond})
		if i%2 == 0 {
			calls = append(calls, statistics.ToolCallObservation{Name: names[i], Executed: true, Status: "error", ErrorCode: "boom", ErrorClass: "transient"})
		}
	}
	c.RecordToolTurn(statistics.ToolTurnObservation{Epoch: c.ToolEpoch(), ChatID: 1, GenerationModel: "m", Advertised: names, Calls: calls})

	return c
}

func decodePNG(t *testing.T, data []byte) (width, height int) {
	t.Helper()
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("not a valid PNG: %v", err)
	}
	bounds := img.Bounds()

	return bounds.Dx(), bounds.Dy()
}

func TestRenderToolsProducesAValidPNG(t *testing.T) {
	c := toolCollectorWith(t, []string{"get_current_time", "search_web", "set_reaction", "unused_tool"}, 3)
	data, err := RenderTools(ToolReport{Snapshot: c.ToolSnapshot(statistics.GlobalScope()), ScopeLabel: "Global"})
	if err != nil {
		t.Fatal(err)
	}
	width, height := decodePNG(t, data)
	if width != reportWidth || height < 400 || height > 1200 {
		t.Fatalf("unexpected size %dx%d", width, height)
	}
}

func TestRenderToolsHeightIsBoundedForLargeRegistries(t *testing.T) {
	small := toolCollectorWith(t, []string{"a", "b", "c"}, 2)
	names := make([]string, 0, 300)
	for i := 0; i < 300; i++ {
		names = append(names, fmt.Sprintf("mcp_server__tool_%03d", i))
	}
	large := toolCollectorWith(t, names, 5)

	smallPNG, err := RenderTools(ToolReport{Snapshot: small.ToolSnapshot(statistics.GlobalScope())})
	if err != nil {
		t.Fatal(err)
	}
	largePNG, err := RenderTools(ToolReport{Snapshot: large.ToolSnapshot(statistics.GlobalScope())})
	if err != nil {
		t.Fatal(err)
	}
	_, smallHeight := decodePNG(t, smallPNG)
	_, largeHeight := decodePNG(t, largePNG)
	limit := canvasTopMargin + toolHeaderHeight + 3*bandPadding + 2*toolPanelPadding + 40 +
		maxToolRows*(toolRowHeight+toolTableRowHeight) + toolTableRowHeight + toolFooterHeight
	if largeHeight > limit || largeHeight <= smallHeight {
		t.Fatalf("height must grow with rows but stay bounded: small %d large %d limit %d", smallHeight, largeHeight, limit)
	}
}

func TestRenderToolsEmptyStateRenders(t *testing.T) {
	c := statistics.NewCollector(statistics.Config{})
	data, err := RenderTools(ToolReport{Snapshot: c.ToolSnapshot(statistics.GlobalScope())})
	if err != nil {
		t.Fatal(err)
	}
	if _, height := decodePNG(t, data); height < 300 {
		t.Fatalf("height %d", height)
	}
}

func TestRenderToolsChatScopeAndOverflowDisclosure(t *testing.T) {
	c := toolCollectorWith(t, []string{"a", "b"}, 2)
	for chat := int64(2); chat < int64(statistics.MaxTrackedChats)+6; chat++ {
		c.RecordToolTurn(statistics.ToolTurnObservation{Epoch: c.ToolEpoch(), ChatID: chat, GenerationModel: "m", Advertised: []string{"a"}})
	}
	for _, scope := range []statistics.InteractionScope{statistics.GlobalScope(), statistics.ChatScope(1), statistics.ChatScope(int64(statistics.MaxTrackedChats) + 4)} {
		data, err := RenderTools(ToolReport{Snapshot: c.ToolSnapshot(scope), Scope: scope, ScopeLabel: "x"})
		if err != nil {
			t.Fatal(err)
		}
		decodePNG(t, data)
	}
}

func TestSelectToolRowsKeepsOrderAndDisclosesOmissions(t *testing.T) {
	names := make([]string, 0, maxToolRows+7)
	for i := 0; i < maxToolRows+7; i++ {
		names = append(names, fmt.Sprintf("tool_%03d", i))
	}
	c := toolCollectorWith(t, names, 3) // three used tools, the rest zero-use
	rows, omitted, omittedZero := selectToolRows(c.ToolSnapshot(statistics.GlobalScope()).Tools)
	if len(rows) != maxToolRows || omitted != 7 || omittedZero != 7 {
		t.Fatalf("rows=%d omitted=%d zero=%d", len(rows), omitted, omittedZero)
	}
	for i := 1; i < len(rows); i++ {
		if rows[i-1].Counters.Attempts < rows[i].Counters.Attempts {
			t.Fatalf("rows must be descending by attempts: %d before %d", rows[i-1].Counters.Attempts, rows[i].Counters.Attempts)
		}
	}
	if rows[0].Counters.Attempts == 0 {
		t.Fatal("used tools come first; zero-use tools fill the remaining rows")
	}
}

func TestNiceUnit(t *testing.T) {
	for in, want := range map[uint64]float64{0: 1, 6: 1, 7: 2, 20: 5, 60: 10, 300: 50, 1000: 200, 123456: 50000} {
		if got := niceUnit(in); got != want {
			t.Errorf("niceUnit(%d) = %v, want %v", in, got, want)
		}
	}
}

func TestToolRowLabelsNeverExposeOverflowAsNames(t *testing.T) {
	name := "real"
	for row, want := range map[string]string{"": "real", "unregistered": "(unregistered names)", "other_registered": "(other registered tools)"} {
		view := statistics.ToolRowView{Overflow: row}
		if row == "" {
			view.Tool = &name
		}
		if got := toolRowLabel(view); got != want {
			t.Errorf("label %q = %q, want %q", row, got, want)
		}
	}
}
