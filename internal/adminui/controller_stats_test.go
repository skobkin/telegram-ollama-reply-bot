package adminui

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"telegram-ollama-reply-bot/internal/adminconfig"
	"telegram-ollama-reply-bot/internal/statistics"
)

// seedInteraction records one full observation set (submission, inference,
// decision, dispatch, execution) for one chat into the test controller's
// collector, under the talk_low profile.
func seedInteraction(t *testing.T, c *Controller, chatID int64, model string) {
	t.Helper()

	keys := statistics.InteractionKeys{ChatID: chatID, Profile: "talk_low", DecisionModel: model, Mode: statistics.ModeLive}
	epoch := c.stats.BeginSubmission(chatID)
	c.stats.RecordInference(statistics.InferenceAttempt{
		Epoch: epoch, Keys: keys, Model: model,
		Latency: 800 * time.Millisecond, ErrClass: statistics.InferenceSuccess,
	})
	c.stats.RecordDecision(statistics.DecisionObservation{
		Epoch: epoch, Keys: keys, Outcome: statistics.OutcomeSelectedReply,
		Scores:          &statistics.ScoreSet{Base: 0.4, Relevance: 0.5, Contribution: 0.6, Intrusion: 0.1, ReactSuitability: 0.7, ReplySuitability: 0.4},
		DecisionLatency: 1500 * time.Millisecond,
	})
	c.stats.RecordExecutionDispatch(statistics.ExecutionDispatch{
		Epoch: epoch, Keys: keys, GenerationModel: "gen-model", Action: statistics.ActionReply,
	})
	c.stats.RecordExecution(statistics.ExecutionObservation{
		Epoch: epoch, Keys: keys, GenerationModel: "gen-model", Action: statistics.ActionReply,
		TextConfirmed: true, Duration: 2 * time.Second,
	})
}

// openStats opens the statistics screen over a fresh dashboard and returns
// the session plus the rendered screen.
func openStats(t *testing.T, c *Controller) (*Session, *Screen) {
	t.Helper()

	session := mustOpen(t, c)
	outcome := tap(t, c, session, VerbStats)
	if outcome.Render == nil || outcome.Render.Screen.Kind != ScreenStatistics {
		t.Fatalf("stats tap must render the statistics screen: %+v", outcome.Render)
	}

	return session, outcome.Render.Built
}

// hasButton reports whether any rendered row carries a button for the verb.
func hasButton(screen *Screen, verb string) bool {
	for _, row := range screen.Rows {
		for _, button := range row {
			if strings.Contains(button.Data, ":"+verb+":") {
				return true
			}
		}
	}

	return false
}

// hasButtonLabel reports whether any rendered button carries the label.
func hasButtonLabel(screen *Screen, label string) bool {
	for _, row := range screen.Rows {
		for _, button := range row {
			if button.Label == label {
				return true
			}
		}
	}

	return false
}

func TestStatisticsScreenShowsInteractionView(t *testing.T) {
	c, _ := newTestController(t)
	seedInteraction(t, c, testGroupID, "m1")

	_, screen := openStats(t, c)
	text := screen.Text

	for _, want := range []string{
		"🔗 Interaction (live, rubric v",
		"Scope: Global", "profile: All", "model: All",
		"Eligible 1", "Evaluated 1", "Executed 1 turns", "texts 1",
		"Without effect 0", "failed 0",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("statistics screen must show %q, got:\n%s", want, text)
		}
	}
	if strings.Contains(text, "⚠️") {
		t.Fatalf("a clean global view must not warn:\n%s", text)
	}
	for _, verb := range []string{VerbStatsScope, VerbStatsProfile, VerbStatsModel, VerbStatsExport, VerbStatsResetI, VerbStatsResetG} {
		if !hasButton(screen, verb) {
			t.Fatalf("statistics screen must carry a %s button", verb)
		}
	}
}

func TestStatisticsScreenUnavailableWithoutCollector(t *testing.T) {
	manager := NewManager(nil)
	service := adminconfig.NewService(newRecordingStore(), nil, nil)
	c := NewController(manager, service, []int64{testAdminID}, nil)

	_, screen := openStats(t, c)
	text := screen.Text
	if !strings.Contains(text, "Statistics are not available.") {
		t.Fatalf("nil collector must keep the unavailable text:\n%s", text)
	}
	if strings.Contains(text, "🔗 Interaction") {
		t.Fatalf("nil collector must not render an interaction view:\n%s", text)
	}
	if hasButton(screen, VerbStatsResetI) {
		t.Fatalf("nil collector must not offer statistics actions")
	}
}

func TestStatisticsProfileFilterCycles(t *testing.T) {
	c, _ := newTestController(t)
	seedInteraction(t, c, testGroupID, "m1")

	session, _ := openStats(t, c)

	// First tap: All → non_interactive. The seeded data lives under
	// talk_low, so the narrowed selection is empty — and must say so
	// instead of claiming an empty dataset or showing bare zero counters.
	outcome := tap(t, c, session, VerbStatsProfile)
	text := outcome.Render.Built.Text
	if !strings.Contains(text, "profile: Non-interactive") {
		t.Fatalf("first profile tap must select non_interactive:\n%s", text)
	}
	if !strings.Contains(text, "No observations match this selection.") {
		t.Fatalf("an empty narrowed selection must be labelled:\n%s", text)
	}
	if strings.Contains(text, "No interaction observations yet.") {
		t.Fatalf("a narrowed selection must not claim an empty dataset:\n%s", text)
	}

	// Cycle to talk_low: the seeded data becomes visible again.
	for i := 0; i < 3; i++ {
		outcome = tap(t, c, session, VerbStatsProfile)
	}
	if !strings.Contains(outcome.Render.Built.Text, "profile: Talk (low)") {
		t.Fatalf("cycling must reach talk_low:\n%s", outcome.Render.Built.Text)
	}
	if !strings.Contains(outcome.Render.Built.Text, "Evaluated 1") {
		t.Fatalf("the matching filter must show the seeded evaluation:\n%s", outcome.Render.Built.Text)
	}

	// Cycle wraps back to All.
	for i := 0; i < 2; i++ {
		outcome = tap(t, c, session, VerbStatsProfile)
	}
	if !strings.Contains(outcome.Render.Built.Text, "profile: All") {
		t.Fatalf("the profile cycle must wrap back to All:\n%s", outcome.Render.Built.Text)
	}
}

func TestStatisticsModelFilterCycles(t *testing.T) {
	c, _ := newTestController(t)
	seedInteraction(t, c, testGroupID, "m1")
	seedInteraction(t, c, 200, "m2")

	session, _ := openStats(t, c)

	outcome := tap(t, c, session, VerbStatsModel)
	if !strings.Contains(outcome.Render.Built.Text, "model: m1") {
		t.Fatalf("first model tap must select the first observed model:\n%s", outcome.Render.Built.Text)
	}

	outcome = tap(t, c, session, VerbStatsModel)
	if !strings.Contains(outcome.Render.Built.Text, "model: m2") {
		t.Fatalf("second model tap must select m2:\n%s", outcome.Render.Built.Text)
	}

	// Wrap back to All after the last observed model.
	outcome = tap(t, c, session, VerbStatsModel)
	if !strings.Contains(outcome.Render.Built.Text, "model: All") {
		t.Fatalf("the model cycle must wrap to All:\n%s", outcome.Render.Built.Text)
	}
	if !strings.Contains(outcome.Render.Built.Text, "Evaluated 2") {
		t.Fatalf("the unfiltered view must show both evaluations:\n%s", outcome.Render.Built.Text)
	}
}

func TestStatisticsScopePickerAndChatScope(t *testing.T) {
	c, store := newTestController(t)
	store.mu.Lock()
	store.catalog = append(store.catalog, adminconfig.ChatCatalogEntry{
		ChatID: 200, ChatType: "private", Title: "Second Chat",
	})
	store.mu.Unlock()
	seedInteraction(t, c, testGroupID, "m1")
	seedInteraction(t, c, 200, "m2")

	session, _ := openStats(t, c)

	outcome := tap(t, c, session, VerbStatsScope)
	if outcome.Render == nil || outcome.Render.Screen.Kind != ScreenStatsScope {
		t.Fatalf("scope tap must open the picker: %+v", outcome.Render)
	}
	picker := outcome.Render.Built
	if !hasButtonLabel(picker, "Group Foo") || !hasButtonLabel(picker, "Second Chat") {
		t.Fatalf("picker must list catalog chats:\n%s", picker.Text)
	}
	if !hasButtonLabel(picker, "✓ Global") {
		t.Fatalf("picker must mark the current global scope")
	}

	// Select the test group: the screen re-renders scoped to it.
	outcome = tap(t, c, session, VerbStatsScopeSet, strconv.FormatInt(testGroupID, 10))
	if outcome.Render == nil || outcome.Render.Screen.Kind != ScreenStatistics {
		t.Fatalf("scope selection must return to the statistics screen: %+v", outcome.Render)
	}
	text := outcome.Render.Built.Text
	if !strings.Contains(text, "Scope: Group Foo") {
		t.Fatalf("chat scope must be labelled:\n%s", text)
	}
	if !strings.Contains(text, "Eligible 1") || strings.Contains(text, "Eligible 2") {
		t.Fatalf("chat scope must exclude other chats' submissions:\n%s", text)
	}

	// Reopen the picker: the selection is marked; going global restores the
	// whole dataset.
	outcome = tap(t, c, session, VerbStatsScope)
	if !hasButtonLabel(outcome.Render.Built, "✓ Group Foo") {
		t.Fatalf("picker must mark the active chat")
	}
	outcome = tap(t, c, session, VerbStatsScopeSet, "g")
	if !strings.Contains(outcome.Render.Built.Text, "Scope: Global") ||
		!strings.Contains(outcome.Render.Built.Text, "Eligible 2") {
		t.Fatalf("global scope must show every chat again:\n%s", outcome.Render.Built.Text)
	}
}

func TestStatsExportDeliversScopedJSON(t *testing.T) {
	c, _ := newTestController(t)
	seedInteraction(t, c, testGroupID, "m1")

	session, _ := openStats(t, c)

	// Scope to the chat, filter to its only observed model, then export.
	tap(t, c, session, VerbStatsScope)
	tap(t, c, session, VerbStatsScopeSet, strconv.FormatInt(testGroupID, 10))
	tap(t, c, session, VerbStatsModel)
	outcome := tap(t, c, session, VerbStatsExport)

	if outcome.Document == nil {
		t.Fatalf("export must deliver a JSON document")
	}
	if !strings.HasPrefix(outcome.Document.Filename, "interaction-stats-") ||
		!strings.HasSuffix(outcome.Document.Filename, ".json") {
		t.Fatalf("export filename wrong: %q", outcome.Document.Filename)
	}

	var decoded struct {
		Schema  string `json:"schema"`
		Version int    `json:"version"`
		Scope   struct {
			Kind   string `json:"kind"`
			ChatID *int64 `json:"chat_id"`
		} `json:"scope"`
		Filters struct {
			DecisionModel string `json:"decision_model"`
		} `json:"filters_applied"`
		Submission *struct{} `json:"submission"`
		Notes      struct {
			OmittedUnderFilters []string `json:"omitted_under_filters"`
		} `json:"notes"`
	}
	if err := json.Unmarshal(outcome.Document.Content, &decoded); err != nil {
		t.Fatalf("export must decode: %v", err)
	}
	if decoded.Schema != statistics.InteractionSchemaName || decoded.Version != statistics.InteractionSchemaVersion {
		t.Fatalf("export schema/version wrong: %s v%d", decoded.Schema, decoded.Version)
	}
	if decoded.Scope.Kind != "chat" || decoded.Scope.ChatID == nil || *decoded.Scope.ChatID != testGroupID {
		t.Fatalf("export must carry the session scope: %+v", decoded.Scope)
	}
	if decoded.Filters.DecisionModel != "m1" {
		t.Fatalf("export must carry the active model filter: %+v", decoded.Filters)
	}
	if decoded.Submission != nil || len(decoded.Notes.OmittedUnderFilters) != 2 {
		t.Fatalf("model-filtered export must omit the chat-keyed sections: %+v", decoded.Notes)
	}
}

func TestStatsResetsAreConfirmedAndGlobal(t *testing.T) {
	c, _ := newTestController(t)
	seedInteraction(t, c, testGroupID, "m1")
	c.stats.GroupRequest()
	c.stats.PrivateRequest()

	session, _ := openStats(t, c)

	// The interaction reset asks with the global blast radius stated.
	outcome := tap(t, c, session, VerbStatsResetI)
	if outcome.Render == nil || outcome.Render.Screen.Kind != ScreenConfirm {
		t.Fatalf("reset must confirm: %+v", outcome.Render)
	}
	if !strings.Contains(outcome.Render.Built.Text, "GLOBAL") {
		t.Fatalf("the confirm description must state the global scope:\n%s", outcome.Render.Built.Text)
	}

	// Declining changes nothing.
	tap(t, c, session, VerbConfirmNo)
	if got := c.stats.InteractionSnapshot(statistics.GlobalScope(), statistics.InteractionFilters{}).Period.Resets; got != 0 {
		t.Fatalf("declined reset must not touch the dataset, resets=%d", got)
	}

	// Confirming resets interaction statistics; the fresh period is empty.
	tap(t, c, session, VerbStatsResetI)
	outcome = tap(t, c, session, VerbConfirmYes)
	if got := c.stats.InteractionSnapshot(statistics.GlobalScope(), statistics.InteractionFilters{}).Period.Resets; got != 1 {
		t.Fatalf("confirmed reset must increment the period resets, got %d", got)
	}
	if !strings.Contains(outcome.Render.Built.Text, "No interaction observations yet.") {
		t.Fatalf("the fresh period must render empty:\n%s", outcome.Render.Built.Text)
	}

	// The general reset zeroes general counters but keeps process uptime.
	c.stats.GroupRequest()
	tap(t, c, session, VerbStatsResetG)
	tap(t, c, session, VerbConfirmYes)
	general := c.stats.GeneralSnapshot()
	if general.GroupRequests != 0 || general.PrivateRequests != 0 {
		t.Fatalf("general reset must zero the counters: %+v", general)
	}
	if general.Uptime == "" {
		t.Fatalf("general reset must keep the process uptime")
	}
	// The interaction dataset survives a general reset.
	if got := c.stats.InteractionSnapshot(statistics.GlobalScope(), statistics.InteractionFilters{}).Period.Resets; got != 1 {
		t.Fatalf("general reset must not touch interaction statistics, resets=%d", got)
	}
}

func TestStatsActionsWithoutCollectorAreRejected(t *testing.T) {
	manager := NewManager(nil)
	service := adminconfig.NewService(newRecordingStore(), nil, nil)
	c := NewController(manager, service, []int64{testAdminID}, nil)

	session := mustOpen(t, c)
	session.mu.Lock()
	seq, msgID := session.Seq, session.MsgID
	session.mu.Unlock()

	// Invalid statistics actions are handled outcomes ("Unknown action"),
	// never propagated errors and never a crash.
	for _, verb := range []string{VerbStatsExport, VerbStatsResetI, VerbStatsResetG, VerbStatsModel} {
		outcome, err := c.HandleCallback(context.Background(), testAdminID, msgID, EncodeAction(verb, seq))
		if err != nil {
			t.Fatalf("%s without a collector: unexpected error %v", verb, err)
		}
		if outcome.Answer != AnswerUnknown {
			t.Fatalf("%s without a collector must answer %q, got %q", verb, AnswerUnknown, outcome.Answer)
		}
	}
}

func TestStatisticsWarnsOnFoldedSelection(t *testing.T) {
	c, _ := newTestController(t)

	// Fill the chat cap so overflow folding becomes active.
	for chat := int64(1); chat <= statistics.MaxTrackedChats+3; chat++ {
		seedInteraction(t, c, chat, "m1")
	}

	session, _ := openStats(t, c)

	// The profile filter selects a profile with no tracked buckets: bare
	// zeros are never presented as truth.
	outcome := tap(t, c, session, VerbStatsProfile)
	if !strings.Contains(outcome.Render.Built.Text, "⚠️") {
		t.Fatalf("a folded empty selection must warn:\n%s", outcome.Render.Built.Text)
	}
}
