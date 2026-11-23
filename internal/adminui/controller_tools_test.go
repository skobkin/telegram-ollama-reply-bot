package adminui

import (
	"strconv"
	"strings"
	"testing"
)

// toChatCard opens the chat card of the test group from the dashboard.
func toChatCard(t *testing.T, c *Controller, session *Session) Outcome {
	t.Helper()

	tap(t, c, session, VerbCatalog, "1")
	outcome := tap(t, c, session, VerbChat, strconv.FormatInt(testGroupID, 10))
	if outcome.Render == nil || outcome.Render.Screen.Kind != ScreenChatCard {
		t.Fatalf("chat tap must render the card: %+v", outcome.Render)
	}

	return outcome
}

func requireToolsScreen(t *testing.T, outcome Outcome) *Screen {
	t.Helper()

	if outcome.Render == nil || outcome.Render.Screen.Kind != ScreenTools {
		t.Fatalf("expected the tools screen, got %+v", outcome.Render)
	}

	return outcome.Render.Built
}

// The Tools submenu owns the tool-related settings: they leave the settings
// button grid, stay visible in the settings text, and are edited from the
// submenu through the regular field editor.
func TestToolsSubmenuGlobalFlow(t *testing.T) {
	c, store := newTestController(t)
	store.global.WebSearchEnabled = true
	store.global.KeepToolHistory = true
	session := mustOpen(t, c)

	settings := tap(t, c, session, VerbGlobalSet).Render.Built
	buttons := screenButtons(settings)
	if !strings.Contains(buttons, "🛠 Tools") {
		t.Fatalf("global settings must offer the Tools submenu, got %q", buttons)
	}
	for _, moved := range []string{"✎ Web search", "✎ Keep tool"} {
		if strings.Contains(buttons, moved) {
			t.Fatalf("%q must move out of the settings grid, got %q", moved, buttons)
		}
	}
	if !strings.Contains(settings.Text, "Web search") || !strings.Contains(settings.Text, "Keep tool history") {
		t.Fatalf("moved fields must stay visible in the settings text: %q", settings.Text)
	}

	tools := requireToolsScreen(t, tap(t, c, session, VerbTools))
	for _, want := range []string{"Tools — global", "Web search: On", "Keep tool history: On", "paid provider resources"} {
		if !strings.Contains(tools.Text, want) {
			t.Fatalf("tools screen missing %q: %q", want, tools.Text)
		}
	}
	if buttons := screenButtons(tools); !strings.Contains(buttons, "✎ Web search") || !strings.Contains(buttons, "✎ Keep tool") {
		t.Fatalf("tools screen must offer both field editors, got %q", buttons)
	}

	editor := tap(t, c, session, VerbField, "ws")
	if editor.Render == nil || editor.Render.Screen.Kind != ScreenFieldEditor {
		t.Fatalf("field tap must open the editor: %+v", editor.Render)
	}
	tap(t, c, session, VerbValue, "ws", "off")
	store.assertCalls(t, "SetGlobalField:web_search_enabled=false")

	tools = requireToolsScreen(t, tap(t, c, session, VerbBack))
	if !strings.Contains(tools.Text, "Web search: Off") {
		t.Fatalf("tools screen must reflect the saved value: %q", tools.Text)
	}
}

// In chat scope the screen shows whether the value is inherited or
// overridden, plus the global value an inheriting chat would use, and the
// field editor can set, flip and clear the override.
func TestToolsSubmenuChatOverrideFlow(t *testing.T) {
	c, store := newTestController(t)
	store.global.WebSearchEnabled = true
	session := mustOpen(t, c)
	chat := strconv.FormatInt(testGroupID, 10)

	card := toChatCard(t, c, session)
	if !strings.Contains(screenButtons(card.Render.Built), "🛠 Tools") {
		t.Fatalf("chat card must offer the Tools submenu, got %q", screenButtons(card.Render.Built))
	}

	tools := requireToolsScreen(t, tap(t, c, session, VerbTools))
	if !strings.Contains(tools.Text, "Web search: On (inherited)") {
		t.Fatalf("an untouched chat must show an inherited value: %q", tools.Text)
	}

	tap(t, c, session, VerbField, "ws")
	tap(t, c, session, VerbValue, "ws", "off")
	tools = requireToolsScreen(t, tap(t, c, session, VerbBack))
	if !strings.Contains(tools.Text, "Web search: Off [chat override] (global: On)") {
		t.Fatalf("override must show its source and the global value: %q", tools.Text)
	}

	tap(t, c, session, VerbField, "ws")
	tap(t, c, session, VerbValue, "ws", "inh")
	tools = requireToolsScreen(t, tap(t, c, session, VerbBack))
	if !strings.Contains(tools.Text, "Web search: On (inherited)") {
		t.Fatalf("clearing the override must return to inheritance: %q", tools.Text)
	}

	store.assertCalls(t,
		"SetChatField:"+chat+":web_search_enabled=false",
		"ClearChatField:"+chat+":web_search_enabled",
	)
}

// Keep tool history only matters while live previews are on, so the chat
// Tools screen hides it with the preview master switch, like the settings
// screens do. Stored overrides are untouched.
func TestToolsSubmenuKeepToolHistoryFollowsPreviewMasterSwitch(t *testing.T) {
	for _, tc := range []struct {
		name     string
		previews bool
		want     bool
	}{
		{"previews off", false, false},
		{"previews on", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, store := newTestController(t)
			store.global.ShowReplyPreviews = tc.previews
			session := mustOpen(t, c)

			toChatCard(t, c, session)
			tools := requireToolsScreen(t, tap(t, c, session, VerbTools))

			if got := strings.Contains(tools.Text, "Keep tool history"); got != tc.want {
				t.Fatalf("keep tool history shown = %v, want %v: %q", got, tc.want, tools.Text)
			}
			if !strings.Contains(tools.Text, "Web search") {
				t.Fatalf("web search must stay available: %q", tools.Text)
			}
		})
	}
}

// With no search backend in the environment the setting is hidden everywhere
// it would be offered; the screen never claims a capability that is absent.
func TestToolsSubmenuHidesWebSearchWithoutBackend(t *testing.T) {
	c, store := newTestController(t, WithHiddenWebSearch())
	store.global.KeepToolHistory = true
	session := mustOpen(t, c)

	settings := tap(t, c, session, VerbGlobalSet).Render.Built
	if strings.Contains(settings.Text, "Web search") {
		t.Fatalf("global settings must hide web search: %q", settings.Text)
	}
	tools := requireToolsScreen(t, tap(t, c, session, VerbTools))
	if strings.Contains(tools.Text, "Web search") || strings.Contains(screenButtons(tools), "Web search") {
		t.Fatalf("global tools screen must hide web search: %q", tools.Text)
	}
	if !strings.Contains(tools.Text, "Keep tool history") {
		t.Fatalf("other tool settings must remain: %q", tools.Text)
	}

	tap(t, c, session, VerbHome)
	card := toChatCard(t, c, session)
	if strings.Contains(card.Render.Built.Text, "Web search") {
		t.Fatalf("chat card must hide web search: %q", card.Render.Built.Text)
	}
	chatSettings := tap(t, c, session, VerbChatSet).Render.Built
	if strings.Contains(chatSettings.Text, "Web search") {
		t.Fatalf("chat settings must hide web search: %q", chatSettings.Text)
	}

	// Previews are off in this fixture, so nothing is left to configure.
	tap(t, c, session, VerbBack)
	empty := requireToolsScreen(t, tap(t, c, session, VerbTools))
	if !strings.Contains(empty.Text, "No tool settings are available") {
		t.Fatalf("an empty tools screen must say so: %q", empty.Text)
	}
	if strings.Contains(screenButtons(empty), "✎") {
		t.Fatalf("an empty tools screen must offer no editors: %q", screenButtons(empty))
	}
}

// Both scopes list the Tools settings in the same order, so the editor buttons
// do not swap rows between the global and the chat screen.
func TestToolsSubmenuListsSettingsInTheSameOrderInBothScopes(t *testing.T) {
	c, store := newTestController(t)
	store.global.ShowReplyPreviews = true
	session := mustOpen(t, c)

	positions := func(screen *Screen) (web, keep int) {
		buttons := screenButtons(screen)

		return strings.Index(buttons, "✎ Web search"), strings.Index(buttons, "✎ Keep tool")
	}

	tap(t, c, session, VerbGlobalSet)
	globalWeb, globalKeep := positions(requireToolsScreen(t, tap(t, c, session, VerbTools)))

	tap(t, c, session, VerbHome)
	toChatCard(t, c, session)
	chatWeb, chatKeep := positions(requireToolsScreen(t, tap(t, c, session, VerbTools)))

	if globalWeb < 0 || globalKeep < 0 || chatWeb < 0 || chatKeep < 0 {
		t.Fatalf("both screens must offer both editors: global=%d/%d chat=%d/%d", globalWeb, globalKeep, chatWeb, chatKeep)
	}
	if (globalWeb < globalKeep) != (chatWeb < chatKeep) {
		t.Fatalf("the global and chat Tools screens list the settings in a different order")
	}
}
