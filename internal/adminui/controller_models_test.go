package adminui

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"telegram-ollama-reply-bot/internal/adminconfig"
)

// stubDiscovery serves a fixed model list (or error) for the picker tests.
type stubDiscovery struct {
	models []string
	err    error
	calls  int
}

func (d *stubDiscovery) ListModels(context.Context) ([]string, error) {
	d.calls++

	return d.models, d.err
}

// newDiscoveringController wires a controller with per-feature discovery
// stubs, mirroring the production option wiring.
func newDiscoveringController(t *testing.T, chat, image, systemOne ModelDiscovery) (*Controller, *recordingStore) {
	t.Helper()

	return newTestController(t, WithModelDiscovery(chat, image, systemOne))
}

// toGlobalSettings navigates an open dashboard to the global settings screen.
func toGlobalSettings(t *testing.T, c *Controller, session *Session) {
	t.Helper()

	outcome := tap(t, c, session, VerbGlobalSet)
	if outcome.Render == nil || outcome.Render.Screen.Kind != ScreenGlobalSet {
		t.Fatalf("global settings tap must render the screen: %+v", outcome.Render)
	}
}

// toFieldEditor navigates to a global field editor and returns its render.
func toFieldEditor(t *testing.T, c *Controller, session *Session, code string) Outcome {
	t.Helper()
	toGlobalSettings(t, c, session)

	outcome := tap(t, c, session, VerbField, code)
	if outcome.Render == nil || outcome.Render.Screen.Kind != ScreenFieldEditor {
		t.Fatalf("field tap must open the editor: %+v", outcome.Render)
	}

	return outcome
}

// TestModelPickerFlow pins the index-based picker: the backend list is
// fetched once, cached into the session, rendered as index buttons (model IDs
// overflow callback data), and a pick applies the selected model.
func TestModelPickerFlow(t *testing.T) {
	discovery := &stubDiscovery{models: []string{"zeta-9b", "alpha-70b", "mid-8b"}}
	c, store := newDiscoveringController(t, discovery, discovery, nil)
	session := mustOpen(t, c)

	outcome := toFieldEditor(t, c, session, "cm")
	// The fetch button shows from the start (discovery is wired); model IDs
	// must not render before the list was actually fetched.
	before := screenButtons(outcome.Render.Built) + "\n" + outcome.Render.Built.Text
	if strings.Contains(before, "alpha-70b") {
		t.Fatalf("model buttons must only appear after a fetch, got %q", before)
	}

	outcome = tap(t, c, session, VerbModelList, "cm")
	if discovery.calls != 1 {
		t.Fatalf("expected exactly one discovery call, got %d", discovery.calls)
	}
	built := outcome.Render.Built
	buttons := screenButtons(built)
	for _, model := range []string{"alpha-70b", "mid-8b", "zeta-9b"} {
		if !strings.Contains(buttons, model) {
			t.Fatalf("sorted list must show %q, got %q", model, buttons)
		}
	}
	if !strings.Contains(built.Text, "availability, not capability") {
		t.Fatalf("picker must state that discovery is not capability: %q", built.Text)
	}
	if !strings.Contains(screenButtons(built), "✏️ Enter model ID…") {
		t.Fatalf("manual entry must stay available: %q", screenButtons(built))
	}

	// The second-sorted model (index 0) applies on pick.
	outcome = tap(t, c, session, VerbModelPick, "cm", "0")
	if outcome.Render.Screen.Kind != ScreenFieldEditor {
		t.Fatalf("pick must stay on the editor: %+v", outcome.Render.Screen)
	}
	if !strings.Contains(outcome.Render.Built.Text, "Saved.") {
		t.Fatalf("pick must acknowledge: %q", outcome.Render.Built.Text)
	}
	store.assertCalls(t, "SetGlobalField:chat_model=alpha-70b")

	// The cache survives the pick: another pick needs no second fetch.
	outcome = tap(t, c, session, VerbModelPick, "cm", "2")
	store.assertCalls(t, "SetGlobalField:chat_model=alpha-70b", "SetGlobalField:chat_model=zeta-9b")
	if discovery.calls != 1 {
		t.Fatalf("cached list must not refetch, got %d calls", discovery.calls)
	}
}

// TestModelPickerOutOfBoundIndexRejected pins that only cached indices
// apply: a stale or forged index never writes.
func TestModelPickerOutOfBoundIndexRejected(t *testing.T) {
	discovery := &stubDiscovery{models: []string{"alpha-70b"}}
	c, store := newDiscoveringController(t, discovery, discovery, discovery)
	session := mustOpen(t, c)

	toFieldEditor(t, c, session, "so")
	tap(t, c, session, VerbModelList, "so")

	outcome := tap(t, c, session, VerbModelPick, "so", "5")
	if outcome.Answer != AnswerUnknown {
		t.Fatalf("out-of-bound index must map to unknown action, got %q", outcome.Answer)
	}
	if len(store.writeCalls()) != 0 {
		t.Fatalf("no store write may happen, got %v", store.writeCalls())
	}

	outcome = tap(t, c, session, VerbModelPick, "so", "-1")
	if outcome.Answer != AnswerUnknown {
		t.Fatalf("negative index must map to unknown action, got %q", outcome.Answer)
	}
}

// TestModelPickerDiscoveryUnavailableAndFailing covers the two degradation
// paths: no provider wired, and a failing provider. Both are notices on the
// editor with manual entry intact — never error screens, never writes.
func TestModelPickerDiscoveryUnavailableAndFailing(t *testing.T) {
	t.Run("no provider wired", func(t *testing.T) {
		c, store := newTestController(t)
		session := mustOpen(t, c)

		outcome := toFieldEditor(t, c, session, "iv")
		if strings.Contains(screenButtons(outcome.Render.Built), "Backend models") {
			t.Fatalf("no provider means no picker button: %q", screenButtons(outcome.Render.Built))
		}

		outcome = tap(t, c, session, VerbModelList, "iv")
		if !strings.Contains(outcome.Render.Built.Text, "No backend list available") {
			t.Fatalf("expected unavailability notice: %q", outcome.Render.Built.Text)
		}
		if len(store.writeCalls()) != 0 {
			t.Fatalf("no store writes expected, got %v", store.writeCalls())
		}
	})

	t.Run("provider fails", func(t *testing.T) {
		discovery := &stubDiscovery{err: errors.New("backend down")}
		c, store := newDiscoveringController(t, discovery, discovery, discovery)
		session := mustOpen(t, c)

		toFieldEditor(t, c, session, "cm")
		outcome := tap(t, c, session, VerbModelList, "cm")
		if !strings.Contains(outcome.Render.Built.Text, "unavailable") {
			t.Fatalf("expected failure notice: %q", outcome.Render.Built.Text)
		}
		if strings.Contains(outcome.Render.Built.Text, "backend down") {
			t.Fatalf("transport detail must never reach the screen: %q", outcome.Render.Built.Text)
		}
		if len(store.writeCalls()) != 0 {
			t.Fatalf("no store writes expected, got %v", store.writeCalls())
		}
	})
}

// TestModelPickerPagination pins index-stable paging: page 2 buttons carry
// absolute list indices, so a pick applies the right model regardless of the
// page shown.
func TestModelPickerPagination(t *testing.T) {
	models := make([]string, 0, 20)
	for i := range 20 {
		models = append(models, fmt.Sprintf("model-%02d", i))
	}
	discovery := &stubDiscovery{models: models}
	c, _ := newDiscoveringController(t, discovery, discovery, discovery)
	session := mustOpen(t, c)

	toFieldEditor(t, c, session, "sm")
	tap(t, c, session, VerbModelList, "sm")

	outcome := tap(t, c, session, VerbModelPage, "2")
	labels := screenButtons(outcome.Render.Built)
	if !strings.Contains(labels, "2/3") {
		t.Fatalf("expected a 3-page pager, got %q", labels)
	}
	// sorted list is model-00..model-19; page 2 shows model-08..model-15
	for _, want := range []string{"model-08", "model-15"} {
		if !strings.Contains(labels, want) {
			t.Fatalf("page 2 must show %q, got %q", want, labels)
		}
	}
	if strings.Contains(labels, "model-00") {
		t.Fatalf("page 1 models must not leak onto page 2: %q", labels)
	}

	// Picking the first button of page 2 applies absolute index 8.
	outcome = tap(t, c, session, VerbModelPick, "sm", "8")
	if outcome.Answer == AnswerUnknown {
		t.Fatalf("in-range page-2 pick must apply")
	}
}

// TestChatScopeModelPicker pins the chat override path: the picker on a chat
// field editor writes the chat override, not a global value.
func TestChatScopeModelPicker(t *testing.T) {
	discovery := &stubDiscovery{models: []string{"julia-1", "laya"}}
	c, store := newDiscoveringController(t, discovery, discovery, discovery)
	session := mustOpen(t, c)

	tap(t, c, session, VerbCatalog, "1")
	tap(t, c, session, VerbChat, strconv.FormatInt(testGroupID, 10))
	outcome := tap(t, c, session, VerbChatSet)
	if outcome.Render == nil || outcome.Render.Screen.Kind != ScreenChatSet {
		t.Fatalf("chat settings tap must render: %+v", outcome.Render)
	}

	outcome = tap(t, c, session, VerbField, "so")
	if outcome.Render.Screen.Kind != ScreenFieldEditor {
		t.Fatalf("field tap must open the editor: %+v", outcome.Render)
	}

	tap(t, c, session, VerbModelList, "so")
	outcome = tap(t, c, session, VerbModelPick, "so", "1")
	if outcome.Answer == AnswerUnknown {
		t.Fatalf("chat-scope pick must apply")
	}
	store.assertCalls(t, "SetChatField:"+strconv.FormatInt(testGroupID, 10)+":systemone_model=laya")
}

// TestClearGlobalModelSemantics pins the two clear verb labels and effects:
// generation models reset to the environment route, the System One model
// clears the global default with per-chat overrides surviving.
func TestClearGlobalModelSemantics(t *testing.T) {
	discovery := &stubDiscovery{models: []string{"gemma4"}}
	c, store := newDiscoveringController(t, discovery, discovery, discovery)
	store.global.ChatModel = "gemma4:e4b" // seeded, not a recorded write
	store.global.SystemOneModel = "laya"
	session := mustOpen(t, c)

	outcome := toFieldEditor(t, c, session, "cm")
	if !strings.Contains(screenButtons(outcome.Render.Built), "Reset to ENV default") {
		t.Fatalf("generation model editor must offer the ENV reset: %q", screenButtons(outcome.Render.Built))
	}

	outcome = tap(t, c, session, VerbClearGlobal, "cm")
	if !strings.Contains(outcome.Render.Built.Text, "environment route") {
		t.Fatalf("reset notice unexpected: %q", outcome.Render.Built.Text)
	}
	if store.global.ChatModel != "" {
		t.Fatalf("generation model must clear to empty, got %q", store.global.ChatModel)
	}

	// Navigate to the SO model editor and clear there.
	tap(t, c, session, VerbBack)
	outcome = tap(t, c, session, VerbField, "so")
	if !strings.Contains(screenButtons(outcome.Render.Built), "Clear global default model") {
		t.Fatalf("SO editor must offer the distinct clear label: %q", screenButtons(outcome.Render.Built))
	}
	outcome = tap(t, c, session, VerbClearGlobal, "so")
	if !strings.Contains(outcome.Render.Built.Text, "chats with explicit overrides keep theirs") {
		t.Fatalf("SO clear notice unexpected: %q", outcome.Render.Built.Text)
	}
	if store.global.SystemOneModel != "" {
		t.Fatalf("SO model must clear to empty, got %q", store.global.SystemOneModel)
	}

	// An unset model offers no clear button.
	builtCM := toFieldEditor(t, c, session, "cm").Render.Built
	if strings.Contains(screenButtons(builtCM), "Reset to ENV default") {
		t.Fatalf("unset model must not offer a clear, got %q", screenButtons(builtCM))
	}
	builtSO := tap(t, c, session, VerbField, "so").Render.Built
	if strings.Contains(screenButtons(builtSO), "Clear global default model") {
		t.Fatalf("unset SO model must not offer a clear, got %q", screenButtons(builtSO))
	}
}

// TestGlobalProfileChangeRequiresConfirmation pins the scope confirmation:
// enabling an interactive profile as the global default needs an explicit
// confirm naming the inheriting-chat count, and nothing is written before it.
func TestGlobalProfileChangeRequiresConfirmation(t *testing.T) {
	c, store := newTestController(t)
	session := mustOpen(t, c)

	toGlobalSettings(t, c, session)
	outcome := tap(t, c, session, VerbInteractivity)
	if outcome.Render == nil || outcome.Render.Screen.Kind != ScreenInteractivity {
		t.Fatalf("interactivity tap must open the submenu: %+v", outcome.Render)
	}
	if !strings.Contains(outcome.Render.Built.Text, "Non-interactive") {
		t.Fatalf("submenu must show the effective profile: %q", outcome.Render.Built.Text)
	}
	if !strings.Contains(outcome.Render.Built.Text, "(unconfigured") {
		t.Fatalf("unset decision model must read as unconfigured: %q", outcome.Render.Built.Text)
	}

	// Enabling a talk profile goes through confirmation; no write yet.
	outcome = tap(t, c, session, VerbValue, "ip", string(adminconfig.ProfileTalkLow))
	if outcome.Render == nil || outcome.Render.Screen.Kind != ScreenConfirm {
		t.Fatalf("global enable must show the confirm screen: %+v", outcome.Render)
	}
	if !strings.Contains(outcome.Render.Built.Text, "1 chat(s)") {
		t.Fatalf("confirmation must name the inheriting-chat count: %q", outcome.Render.Built.Text)
	}
	if len(store.writeCalls()) != 0 {
		t.Fatalf("no store write before confirmation, got %v", store.writeCalls())
	}

	// Cancelling keeps the default untouched.
	outcome = tap(t, c, session, VerbConfirmNo)
	if len(store.writeCalls()) != 0 {
		t.Fatalf("cancel must not write, got %v", store.writeCalls())
	}

	// Re-do it and confirm: the write happens now.
	tap(t, c, session, VerbValue, "ip", string(adminconfig.ProfileTalkLow))
	outcome = tap(t, c, session, VerbConfirmYes)
	if !strings.Contains(outcome.Render.Built.Text, "Talk (low)") {
		t.Fatalf("submenu must reflect the new default: %q", outcome.Render.Built.Text)
	}
	store.assertCalls(t, "SetGlobalField:default_interaction_profile=talk_low")
}

// TestGlobalProfileToNonInteractiveAppliesDirectly pins that returning to
// the non-interactive default is a direct set without confirmation.
func TestGlobalProfileToNonInteractiveAppliesDirectly(t *testing.T) {
	c, store := newTestController(t)
	store.global.DefaultInteractionProfile = adminconfig.ProfileTalkHigh // seeded, not a recorded write
	session := mustOpen(t, c)

	toGlobalSettings(t, c, session)
	tap(t, c, session, VerbInteractivity)

	outcome := tap(t, c, session, VerbValue, "ip", string(adminconfig.ProfileNonInteractive))
	if outcome.Render == nil || outcome.Render.Screen.Kind != ScreenInteractivity {
		t.Fatalf("disabling must apply directly on the submenu: %+v", outcome.Render)
	}
	store.assertCalls(t, "SetGlobalField:default_interaction_profile=non_interactive")
}

// TestChatProfileChangeAppliesDirectly pins that per-chat profile changes
// need no confirmation and write the chat override.
func TestChatProfileChangeAppliesDirectly(t *testing.T) {
	c, store := newTestController(t)
	session := mustOpen(t, c)

	tap(t, c, session, VerbCatalog, "1")
	tap(t, c, session, VerbChat, strconv.FormatInt(testGroupID, 10))
	outcome := tap(t, c, session, VerbInteractivity)
	if outcome.Render == nil || outcome.Render.Screen.Kind != ScreenInteractivity {
		t.Fatalf("chat interactivity tap must open the submenu: %+v", outcome.Render)
	}

	outcome = tap(t, c, session, VerbValue, "ip", string(adminconfig.ProfileReactionsHigh))
	if outcome.Render == nil || outcome.Render.Screen.Kind != ScreenInteractivity {
		t.Fatalf("chat profile change must apply on the submenu without confirmation: %+v", outcome.Render)
	}
	if strings.Contains(outcome.Render.Built.Text, "Confirm") {
		t.Fatalf("no confirmation may appear for chat scope: %q", outcome.Render.Built.Text)
	}
	store.assertCalls(t, "SetChatField:"+strconv.FormatInt(testGroupID, 10)+":interaction_profile=reactions_high")
	if !strings.Contains(outcome.Render.Built.Text, "Reactions (high) [chat override]") {
		t.Fatalf("submenu must flag the chat override: %q", outcome.Render.Built.Text)
	}
}

// TestInteractivitySubmenuWithDiscovery pins the configured-backend rendering:
// the decision model shows, discovery is reachable, and the global clear
// instructions action exists when instructions are set.
func TestInteractivitySubmenuWithDiscovery(t *testing.T) {
	discovery := &stubDiscovery{models: []string{"laya"}}
	c, store := newDiscoveringController(t, discovery, discovery, discovery)
	store.global.DecisionInstructions = "Engage with programming topics." // seeded, not a recorded write
	session := mustOpen(t, c)

	toGlobalSettings(t, c, session)
	tap(t, c, session, VerbInteractivity)

	// The model editor opens from the submenu with chat/global scope intact.
	outcome := tap(t, c, session, VerbField, "so")
	if outcome.Render == nil || outcome.Render.Screen.Kind != ScreenFieldEditor {
		t.Fatalf("model editor must open from the submenu: %+v", outcome.Render)
	}
	tap(t, c, session, VerbModelList, "so")
	outcome = tap(t, c, session, VerbModelPick, "so", "0")
	store.assertCalls(t, "SetGlobalField:systemone_model=laya")
	if !strings.Contains(outcome.Render.Built.Text, "laya") {
		t.Fatalf("editor must show the picked model: %q", outcome.Render.Built.Text)
	}

	// Back to the submenu: model and instructions now render concretely.
	outcome = tap(t, c, session, VerbBack)
	if outcome.Render == nil || outcome.Render.Screen.Kind != ScreenInteractivity {
		t.Fatalf("back must land on the submenu: %+v", outcome.Render)
	}
	if !strings.Contains(outcome.Render.Built.Text, "laya") {
		t.Fatalf("submenu must show the decision model: %q", outcome.Render.Built.Text)
	}
	if !strings.Contains(screenButtons(outcome.Render.Built), "Clear instructions") {
		t.Fatalf("set global instructions must offer the clear: %q", screenButtons(outcome.Render.Built))
	}

	outcome = tap(t, c, session, VerbClearGlobal, "di")
	if !strings.Contains(outcome.Render.Built.Text, "instructions cleared") {
		t.Fatalf("instructions clear notice unexpected: %q", outcome.Render.Built.Text)
	}
	if store.global.DecisionInstructions != "" {
		t.Fatalf("instructions must clear to empty, got %q", store.global.DecisionInstructions)
	}
}

// TestMultilineInstructionsInput pins the multi-line ForceReply flow: the
// prompt wording announces preserved line breaks and the stored value keeps
// the newlines verbatim.
func TestMultilineInstructionsInput(t *testing.T) {
	c, store := newTestController(t)
	session := mustOpen(t, c)

	toGlobalSettings(t, c, session)
	tap(t, c, session, VerbInteractivity)

	outcome := tap(t, c, session, VerbString, "di")
	if outcome.Input == nil {
		t.Fatalf("instructions edit must request an input prompt")
	}
	if !strings.Contains(outcome.Input.Text, "Line breaks are preserved") {
		t.Fatalf("multiline field must announce preserved breaks: %q", outcome.Input.Text)
	}
	armPending(t, c, 555)

	outcome, err := c.HandleReply(context.Background(), testAdminID, ReplyInput{
		ReplyToMsgID:  555,
		TextAvailable: true,
		Text:          "Engage with programming topics.\nStay polite.",
		ReplyMsgID:    556,
	})
	if err != nil {
		t.Fatalf("handle reply: %v", err)
	}
	if len(store.writeCalls()) != 1 {
		t.Fatalf("expected exactly one store write, got %v", store.writeCalls())
	}
	if store.global.DecisionInstructions != "Engage with programming topics.\nStay polite." {
		t.Fatalf("instructions must store verbatim, got %q", store.global.DecisionInstructions)
	}
}

// TestImageModelHiddenUnderMiniMax pins render-time hiding: with the
// model-free recognizer active, the image model setting disappears from the
// global settings screen while stored values stay untouched.
func TestImageModelHiddenUnderMiniMax(t *testing.T) {
	c, store := newTestController(t, WithHiddenImageModel())
	store.global.ImageRecognitionModel = "vision-mini" // seeded, not a recorded write
	session := mustOpen(t, c)

	toGlobalSettings(t, c, session)
	// Re-render through the notice-free path by tapping into the interactivity
	// submenu and back, which rebuilds the settings screen fresh.
	tap(t, c, session, VerbInteractivity)
	outcome := tap(t, c, session, VerbBack)

	if strings.Contains(outcome.Render.Built.Text, "Image recognition model") {
		t.Fatalf("hidden setting must not render, got %q", outcome.Render.Built.Text)
	}
	if strings.Contains(screenButtons(outcome.Render.Built), "Image recognition") {
		t.Fatalf("hidden setting must have no button: %q", screenButtons(outcome.Render.Built))
	}
	if store.global.ImageRecognitionModel != "vision-mini" {
		t.Fatalf("stored value must survive hiding, got %q", store.global.ImageRecognitionModel)
	}
	// The other model fields stay visible.
	if !strings.Contains(outcome.Render.Built.Text, "Generation model") {
		t.Fatalf("sibling settings must stay visible: %q", outcome.Render.Built.Text)
	}
}

// TestSettingsGroupsInteractivityFields pins the submenu grouping: the
// settings screens no longer carry profile/instruction buttons but route
// through the interactivity entry point instead.
func TestSettingsGroupsInteractivityFields(t *testing.T) {
	c, _ := newTestController(t)
	session := mustOpen(t, c)

	outcome := tap(t, c, session, VerbGlobalSet)
	if outcome.Render == nil || outcome.Render.Screen.Kind != ScreenGlobalSet {
		t.Fatalf("global settings tap must render the screen: %+v", outcome.Render)
	}
	buttons := screenButtons(outcome.Render.Built)
	if !strings.Contains(buttons, "🧠 Interactivity") {
		t.Fatalf("settings screen must offer the interactivity entry: %q", buttons)
	}
	if strings.Contains(buttons, "Interaction profile") || strings.Contains(buttons, "Decision instructions") {
		t.Fatalf("submenu fields must not appear in the settings grid: %q", buttons)
	}
}

// TestChatCardHasInteractivityEntry pins the chat card entry button.
func TestChatCardHasInteractivityEntry(t *testing.T) {
	c, _ := newTestController(t)
	session := mustOpen(t, c)

	tap(t, c, session, VerbCatalog, "1")
	outcome := tap(t, c, session, VerbChat, strconv.FormatInt(testGroupID, 10))
	if !strings.Contains(screenButtons(outcome.Render.Built), "🧠 Interactivity") {
		t.Fatalf("chat card must offer the interactivity entry: %q", screenButtons(outcome.Render.Built))
	}
}
