package adminui

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"telegram-ollama-reply-bot/internal/statistics"
	"telegram-ollama-reply-bot/internal/statistics/pngreport"
)

// seedTools records one turn of tool activity for a chat.
func seedTools(c *Controller, chatID int64, model string) {
	c.stats.RecordToolTurn(statistics.ToolTurnObservation{
		Epoch: c.stats.ToolEpoch(), ChatID: chatID, GenerationModel: model,
		Advertised: []string{"search_web", "get_current_time", "send_poll"},
		Calls: []statistics.ToolCallObservation{
			{Name: "search_web", Executed: true, Status: "error", ErrorCode: "provider_unavailable", ErrorClass: "transient", Duration: 300 * time.Millisecond},
			{Name: "search_web", Executed: true, Status: "ok", Duration: 450 * time.Millisecond},
			{Name: "get_current_time", Executed: true, Status: "empty", Duration: 5 * time.Millisecond},
		},
	})
}

// openToolStats opens the tool screen from the statistics screen.
func openToolStats(t *testing.T, c *Controller) (*Session, *Screen) {
	t.Helper()
	session, _ := openStats(t, c)
	outcome := tap(t, c, session, VerbToolStats)
	if outcome.Render == nil || outcome.Render.Screen.Kind != ScreenToolStats {
		t.Fatalf("the tool tap must render the tool statistics screen: %+v", outcome.Render)
	}

	return session, outcome.Render.Built
}

func TestToolStatisticsScreenShowsSummaryAndControls(t *testing.T) {
	c, _ := newTestController(t)
	c.stats.SetToolCatalog([]string{"search_web", "get_current_time", "send_poll", "never_called"})
	seedTools(c, testGroupID, "gm-a")

	_, screen := openToolStats(t, c)
	for _, verb := range []string{VerbStatsScope, VerbToolExport, VerbStatsResetT, VerbToolStats, VerbBack} {
		if !hasButton(screen, verb) {
			t.Fatalf("the tool screen must offer %q", verb)
		}
	}
	for _, want := range []string{"Tool statistics", "search_web", "Zero-use", "Errors: transient 1", "gm-a", "Persistence:"} {
		if !strings.Contains(screen.Text, want) {
			t.Fatalf("tool screen must contain %q:\n%s", want, screen.Text)
		}
	}
	if len([]rune(screen.Text)) > 3800 {
		t.Fatalf("the screen text must stay within the Telegram budget, got %d runes", len([]rune(screen.Text)))
	}
}

func TestToolStatisticsScreenEmptyAndUnavailableStates(t *testing.T) {
	c, _ := newTestController(t)
	_, screen := openToolStats(t, c)
	if !strings.Contains(screen.Text, "No tool statistics yet.") {
		t.Fatalf("an empty dataset says so:\n%s", screen.Text)
	}

	// A chat scope with nothing recorded says "no observations match".
	session, _ := openToolStats(t, c)
	tap(t, c, session, VerbStatsScope)
	outcome := tap(t, c, session, VerbStatsScopeSet, strconv.FormatInt(testGroupID, 10))
	if !strings.Contains(outcome.Render.Built.Text, "No observations match this selection.") {
		t.Fatalf("a narrowed empty view must not claim an empty dataset:\n%s", outcome.Render.Built.Text)
	}
}

func TestToolStatisticsScopePickerReturnsToTheToolScreenWithThatChat(t *testing.T) {
	c, _ := newTestController(t)
	seedTools(c, testGroupID, "gm-a")
	seedTools(c, 4242, "gm-b")
	session, _ := openToolStats(t, c)

	tap(t, c, session, VerbStatsScope)
	outcome := tap(t, c, session, VerbStatsScopeSet, strconv.FormatInt(testGroupID, 10))
	if outcome.Render.Screen.Kind != ScreenToolStats || outcome.Render.Screen.ChatID != testGroupID {
		t.Fatalf("the picker must return to the tool screen with the chosen chat: %+v", outcome.Render.Screen)
	}
	if strings.Contains(outcome.Render.Built.Text, "gm-b") {
		t.Fatalf("per-chat view must not include the other chat's data:\n%s", outcome.Render.Built.Text)
	}
	if !strings.Contains(outcome.Render.Built.Text, "search_web") {
		t.Fatalf("per-chat view must show the chat's tools:\n%s", outcome.Render.Built.Text)
	}
}

func TestToolStatisticsScreenInheritsTheStatisticsScope(t *testing.T) {
	c, _ := newTestController(t)
	seedTools(c, testGroupID, "gm-a")
	session, _ := openStats(t, c)
	tap(t, c, session, VerbStatsScope)
	tap(t, c, session, VerbStatsScopeSet, strconv.FormatInt(testGroupID, 10))

	outcome := tap(t, c, session, VerbToolStats)
	if outcome.Render.Screen.ChatID != testGroupID {
		t.Fatalf("the tool screen must inherit the chat scope: %+v", outcome.Render.Screen)
	}
}

func captureToolRenderer(captured *[]pngreport.ToolReport, failWith error) ToolReportRenderer {
	return func(report pngreport.ToolReport) ([]byte, error) {
		*captured = append(*captured, report)
		if failWith != nil {
			return nil, failWith
		}

		return []byte("png-bytes"), nil
	}
}

func TestToolStatsExportAttachesPNGAndJSONFromOneSnapshot(t *testing.T) {
	var captured []pngreport.ToolReport
	c, _ := newTestController(t, WithToolStatsReport(captureToolRenderer(&captured, nil)))
	seedTools(c, testGroupID, "gm-a")
	session, _ := openToolStats(t, c)

	outcome := tap(t, c, session, VerbToolExport)
	if len(captured) != 1 {
		t.Fatalf("the renderer must see exactly one report input, got %d", len(captured))
	}
	if outcome.Photo == nil || string(outcome.Photo.Content) != "png-bytes" ||
		!strings.HasPrefix(outcome.Photo.Filename, "tool-stats-") || !strings.HasSuffix(outcome.Photo.Filename, ".png") {
		t.Fatalf("photo %+v", outcome.Photo)
	}
	if outcome.Document == nil || !strings.HasPrefix(outcome.Document.Filename, "tool-stats-") || !strings.HasSuffix(outcome.Document.Filename, ".json") {
		t.Fatalf("document %+v", outcome.Document)
	}
	var decoded struct {
		Schema  string `json:"schema"`
		Version int    `json:"version"`
		Totals  struct {
			Counters struct {
				Attempts uint64 `json:"attempts"`
			} `json:"counters"`
		} `json:"totals"`
	}
	if err := json.Unmarshal(outcome.Document.Content, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Schema != statistics.ToolSchemaName || decoded.Version != statistics.ToolSchemaVersion {
		t.Fatalf("schema %s v%d", decoded.Schema, decoded.Version)
	}
	if decoded.Totals.Counters.Attempts != captured[0].Snapshot.Totals.Counters.Attempts || decoded.Totals.Counters.Attempts != 3 {
		t.Fatalf("PNG input and JSON must carry the same numbers: %d vs %d",
			captured[0].Snapshot.Totals.Counters.Attempts, decoded.Totals.Counters.Attempts)
	}
	if outcome.Message != nil {
		t.Fatalf("a successful render must not notify: %+v", outcome.Message)
	}
}

func TestToolStatsExportWithoutRendererIsJSONOnly(t *testing.T) {
	c, _ := newTestController(t)
	seedTools(c, testGroupID, "gm-a")
	session, _ := openToolStats(t, c)

	outcome := tap(t, c, session, VerbToolExport)
	if outcome.Photo != nil || outcome.Document == nil || outcome.Message != nil {
		t.Fatalf("JSON-only export expected: %+v", outcome)
	}
}

func TestToolStatsExportRenderFailureKeepsJSON(t *testing.T) {
	var captured []pngreport.ToolReport
	c, _ := newTestController(t, WithToolStatsReport(captureToolRenderer(&captured, errors.New("chart exploded: secret-detail"))))
	seedTools(c, testGroupID, "gm-a")
	session, _ := openToolStats(t, c)

	outcome := tap(t, c, session, VerbToolExport)
	if outcome.Photo != nil || outcome.Document == nil {
		t.Fatalf("a render failure must keep the JSON and skip the photo: %+v", outcome)
	}
	if outcome.Message == nil || !strings.Contains(outcome.Message.Text, "PNG report rendering failed") {
		t.Fatalf("the admin must be told: %+v", outcome.Message)
	}
	if strings.Contains(outcome.Message.Text, "secret-detail") {
		t.Fatal("renderer error details must not reach the admin")
	}
}

func TestToolStatsExportIsScopedToTheChat(t *testing.T) {
	c, _ := newTestController(t)
	seedTools(c, testGroupID, "gm-a")
	seedTools(c, 4242, "gm-b")
	session, _ := openToolStats(t, c)
	tap(t, c, session, VerbStatsScope)
	tap(t, c, session, VerbStatsScopeSet, strconv.FormatInt(testGroupID, 10))

	outcome := tap(t, c, session, VerbToolExport)
	var decoded struct {
		Scope struct {
			Kind   string `json:"kind"`
			ChatID *int64 `json:"chat_id"`
		} `json:"scope"`
		Models []struct{} `json:"models"`
	}
	if err := json.Unmarshal(outcome.Document.Content, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Scope.Kind != "chat" || decoded.Scope.ChatID == nil || *decoded.Scope.ChatID != testGroupID {
		t.Fatalf("scope %+v", decoded.Scope)
	}
	if strings.Contains(string(outcome.Document.Content), "gm-b") {
		t.Fatal("a chat-scoped export must not contain another chat's data")
	}
}

func TestToolStatsResetIsConfirmedGlobalAndIndependent(t *testing.T) {
	c, _ := newTestController(t)
	seedInteraction(t, c, testGroupID, "m1")
	c.stats.GroupRequest()
	seedTools(c, testGroupID, "gm-a")
	session, _ := openToolStats(t, c)

	outcome := tap(t, c, session, VerbStatsResetT)
	if outcome.Render == nil || outcome.Render.Screen.Kind != ScreenConfirm || !strings.Contains(outcome.Render.Built.Text, "GLOBAL") {
		t.Fatalf("the reset must confirm with the global blast radius: %+v", outcome.Render)
	}
	tap(t, c, session, VerbConfirmNo)
	if got := c.stats.ToolSnapshot(statistics.GlobalScope()).Period.Resets; got != 0 {
		t.Fatalf("a declined reset changes nothing, resets=%d", got)
	}

	tap(t, c, session, VerbStatsResetT)
	outcome = tap(t, c, session, VerbConfirmYes)
	if got := c.stats.ToolSnapshot(statistics.GlobalScope()).Period.Resets; got != 1 {
		t.Fatalf("resets = %d", got)
	}
	if !strings.Contains(outcome.Render.Built.Text, "No tool statistics yet.") {
		t.Fatalf("the fresh period renders empty:\n%s", outcome.Render.Built.Text)
	}
	// Independence: interaction and general statistics are untouched.
	if got := c.stats.InteractionSnapshot(statistics.GlobalScope(), statistics.InteractionFilters{}).Period.Resets; got != 0 {
		t.Fatalf("the tool reset must not touch interaction statistics, resets=%d", got)
	}
	if c.stats.GeneralSnapshot().GroupRequests != 1 {
		t.Fatal("the tool reset must not touch general statistics")
	}
	// And the other resets must not touch tool statistics.
	seedTools(c, testGroupID, "gm-a")
	st, _ := openStats(t, c)
	tap(t, c, st, VerbStatsResetI)
	tap(t, c, st, VerbConfirmYes)
	tap(t, c, st, VerbStatsResetG)
	tap(t, c, st, VerbConfirmYes)
	if got := c.stats.ToolSnapshot(statistics.GlobalScope()).Totals.Counters.Attempts; got != 3 {
		t.Fatalf("interaction/general resets must not touch tool statistics, attempts=%d", got)
	}
}

func TestToolStatsResetNoticeReflectsDurability(t *testing.T) {
	for _, tc := range []struct {
		name    string
		outcome statistics.DurableOutcome
		want    string
	}{
		{"durable", statistics.DurableOutcome{Durable: true, State: statistics.PersistEnabled}, "Reset saved."},
		{"temporary failure", statistics.DurableOutcome{State: statistics.PersistWriteFailing}, "will be retried"},
		{"disabled", statistics.DurableOutcome{State: statistics.PersistDisabledNewerFormat}, "in memory only"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			persister := &fakeStatsPersister{outcome: tc.outcome}
			c, _ := newTestController(t, WithStatsPersistence(persister))
			seedTools(c, testGroupID, "gm-a")
			session, _ := openToolStats(t, c)

			tap(t, c, session, VerbStatsResetT)
			outcome := tap(t, c, session, VerbConfirmYes)
			if !strings.Contains(outcome.Render.Built.Text, tc.want) {
				t.Fatalf("notice must contain %q:\n%s", tc.want, outcome.Render.Built.Text)
			}
			if len(persister.datasets) != 1 || persister.datasets[0] != statistics.ToolDatasetName {
				t.Fatalf("the tool dataset must be acknowledged: %v", persister.datasets)
			}
		})
	}
}

func TestToolStatisticsAccessControlAndGuards(t *testing.T) {
	c, _ := newTestController(t)
	session, _ := openToolStats(t, c)

	// A non-admin sender is denied before anything else, for every new verb.
	for _, verb := range []string{VerbToolStats, VerbToolExport, VerbStatsResetT} {
		outcome, err := c.HandleCallback(context.Background(), 8, 100, EncodeAction(verb, session.Seq))
		if err != nil || outcome.Answer != AnswerDenied || outcome.Document != nil || outcome.Photo != nil {
			t.Fatalf("%s: non-admin must be denied without side effects: %+v err=%v", verb, outcome, err)
		}
	}
	if got := c.stats.ToolSnapshot(statistics.GlobalScope()).Period.Resets; got != 0 {
		t.Fatal("a denied reset must not run")
	}

	// The export and the reset only exist on the tool screen.
	dashboard := mustOpen(t, c)
	for _, verb := range []string{VerbToolExport, VerbStatsResetT, VerbToolStats} {
		outcome, err := c.HandleCallback(context.Background(), testAdminID, dashboard.MsgID, EncodeAction(verb, dashboard.Seq))
		if err != nil || outcome.Answer != AnswerUnknown || outcome.Document != nil || outcome.Photo != nil {
			t.Fatalf("%s outside the statistics screens must be an unknown action without effects: %+v err=%v", verb, outcome, err)
		}
	}
}

func TestToolStatisticsWithoutCollectorIsRejected(t *testing.T) {
	c, _ := newTestController(t)
	c.stats = nil
	session := mustOpen(t, c)
	for _, verb := range []string{VerbToolStats, VerbToolExport, VerbStatsResetT} {
		// Invalid actions are handled outcomes, never propagated errors.
		outcome, err := c.HandleCallback(context.Background(), testAdminID, session.MsgID, EncodeAction(verb, session.Seq))
		if err != nil || outcome.Answer != AnswerUnknown {
			t.Fatalf("%s without a collector must answer %q, got %q (err %v)", verb, AnswerUnknown, outcome.Answer, err)
		}
	}
}

func TestToolStatisticsWarningsDiscloseFoldingAndPersistenceState(t *testing.T) {
	c, _ := newTestController(t)
	seedTools(c, testGroupID, "gm-a")
	for chat := int64(1); chat <= int64(statistics.MaxTrackedChats)+3; chat++ {
		seedTools(c, chat, "gm-a")
	}
	c.stats.ToolDataset().SetPersistenceInfo(statistics.PersistenceInfo{State: statistics.PersistWriteFailing})

	_, screen := openToolStats(t, c)
	for _, want := range []string{"chat overflow folded", "saving statistics is failing"} {
		if !strings.Contains(screen.Text, want) {
			t.Fatalf("warning %q missing:\n%s", want, screen.Text)
		}
	}
}
