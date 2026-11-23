package adminui

import (
	"strconv"
	"strings"
	"testing"
)

// Image generation is a tool permission, so it lives in the Tools submenu next
// to web search; the generation model stays on the global settings screen.
func TestToolsSubmenuImageGenerationGlobalFlow(t *testing.T) {
	c, store := newTestController(t)
	store.global.ImageGenerationEnabled = true
	session := mustOpen(t, c)

	settings := tap(t, c, session, VerbGlobalSet).Render.Built
	// Grid labels are truncated, so count buttons instead of matching full
	// labels: only the model editor stays in the grid; the permission moved
	// to the Tools submenu.
	if got := strings.Count(screenButtons(settings), "✎ Image generation"); got != 1 {
		t.Fatalf("expected exactly one image generation button (the model) in the settings grid, got %d: %q", got, screenButtons(settings))
	}
	if !strings.Contains(settings.Text, "Image generation: true") {
		t.Fatalf("the permission must stay visible in the settings text: %q", settings.Text)
	}

	tools := requireToolsScreen(t, tap(t, c, session, VerbTools))
	for _, want := range []string{"Image generation: On", "billed per image"} {
		if !strings.Contains(tools.Text, want) {
			t.Fatalf("tools screen missing %q: %q", want, tools.Text)
		}
	}
	if !strings.Contains(screenButtons(tools), "✎ Image generation") {
		t.Fatalf("tools screen must offer the editor, got %q", screenButtons(tools))
	}

	editor := tap(t, c, session, VerbField, "ig")
	if editor.Render == nil || editor.Render.Screen.Kind != ScreenFieldEditor {
		t.Fatalf("field tap must open the editor: %+v", editor.Render)
	}
	tap(t, c, session, VerbValue, "ig", "off")
	store.assertCalls(t, "SetGlobalField:image_generation_enabled=false")

	tools = requireToolsScreen(t, tap(t, c, session, VerbBack))
	if !strings.Contains(tools.Text, "Image generation: Off") {
		t.Fatalf("tools screen must reflect the saved value: %q", tools.Text)
	}
}

func TestToolsSubmenuImageGenerationChatOverrideFlow(t *testing.T) {
	c, store := newTestController(t)
	store.global.ImageGenerationEnabled = true
	session := mustOpen(t, c)
	chat := strconv.FormatInt(testGroupID, 10)

	toChatCard(t, c, session)
	tools := requireToolsScreen(t, tap(t, c, session, VerbTools))
	if !strings.Contains(tools.Text, "Image generation: On (inherited)") {
		t.Fatalf("an untouched chat must show an inherited value: %q", tools.Text)
	}

	tap(t, c, session, VerbField, "ig")
	tap(t, c, session, VerbValue, "ig", "off")
	tools = requireToolsScreen(t, tap(t, c, session, VerbBack))
	if !strings.Contains(tools.Text, "Image generation: Off [chat override] (global: On)") {
		t.Fatalf("override must show its source and the global value: %q", tools.Text)
	}

	tap(t, c, session, VerbField, "ig")
	tap(t, c, session, VerbValue, "ig", "inh")
	tools = requireToolsScreen(t, tap(t, c, session, VerbBack))
	if !strings.Contains(tools.Text, "Image generation: On (inherited)") {
		t.Fatalf("clearing the override must return to inheritance: %q", tools.Text)
	}

	store.assertCalls(t,
		"SetChatField:"+chat+":image_generation_enabled=false",
		"ClearChatField:"+chat+":image_generation_enabled",
	)
}

// Without a generation backend in the environment, neither the permission nor
// the model is offered anywhere: the UI never claims an absent capability.
// Stored values are untouched.
func TestImageGenerationSettingsHiddenWithoutBackend(t *testing.T) {
	c, store := newTestController(t, WithHiddenImageGeneration())
	store.global.ImageGenerationEnabled = true
	store.global.ImageGenerationModel = "MiniMax-H3-Image"
	store.global.ShowReplyPreviews = true
	session := mustOpen(t, c)

	settings := tap(t, c, session, VerbGlobalSet).Render.Built
	if strings.Contains(settings.Text, "Image generation") || strings.Contains(screenButtons(settings), "Image generation") {
		t.Fatalf("global settings must hide image generation: %q", settings.Text)
	}
	tools := requireToolsScreen(t, tap(t, c, session, VerbTools))
	if strings.Contains(tools.Text, "Image generation") || strings.Contains(screenButtons(tools), "Image generation") {
		t.Fatalf("global tools screen must hide image generation: %q", tools.Text)
	}
	if !strings.Contains(tools.Text, "Keep tool history") {
		t.Fatalf("other tool settings must remain: %q", tools.Text)
	}

	tap(t, c, session, VerbHome)
	card := toChatCard(t, c, session)
	if strings.Contains(card.Render.Built.Text, "Image generation") {
		t.Fatalf("chat card must hide image generation: %q", card.Render.Built.Text)
	}
	chatSettings := tap(t, c, session, VerbChatSet).Render.Built
	if strings.Contains(chatSettings.Text, "Image generation") {
		t.Fatalf("chat settings must hide image generation: %q", chatSettings.Text)
	}
	tap(t, c, session, VerbBack)
	chatTools := requireToolsScreen(t, tap(t, c, session, VerbTools))
	if strings.Contains(chatTools.Text, "Image generation") {
		t.Fatalf("chat tools screen must hide image generation: %q", chatTools.Text)
	}

	if !store.global.ImageGenerationEnabled || store.global.ImageGenerationModel != "MiniMax-H3-Image" {
		t.Fatal("hiding is render-time only: stored values must stay untouched")
	}
}

// The generation model is a global, manually entered string with no
// environment route: an unset value reads "(unset)", the editor never offers a
// backend model picker, and a stored value can be cleared.
func TestImageGenerationModelEditor(t *testing.T) {
	c, store := newTestController(t)
	session := mustOpen(t, c)

	settings := tap(t, c, session, VerbGlobalSet).Render.Built
	if !strings.Contains(settings.Text, "Image generation model: (unset)") {
		t.Fatalf("an unset model must render as (unset), got %q", settings.Text)
	}
	if strings.Contains(settings.Text, "Image generation model: (environment route)") {
		t.Fatalf("there is no environment route for the generation model: %q", settings.Text)
	}
	if got := strings.Count(screenButtons(settings), "✎ Image generation"); got != 1 {
		t.Fatalf("the model editor must stay on the settings grid exactly once, got %d: %q", got, screenButtons(settings))
	}

	editor := tap(t, c, session, VerbField, "gm")
	if editor.Render == nil || editor.Render.Screen.Kind != ScreenFieldEditor {
		t.Fatalf("field tap must open the editor: %+v", editor.Render)
	}
	buttons := screenButtons(editor.Render.Built)
	if strings.Contains(buttons, "Backend models") {
		t.Fatalf("the generation model has no backend discovery list: %q", buttons)
	}
	if !strings.Contains(buttons, "Enter model ID") {
		t.Fatalf("manual entry must be offered: %q", buttons)
	}
	if strings.Contains(buttons, "Clear model") {
		t.Fatalf("an unset model has nothing to clear: %q", buttons)
	}

	store.global.ImageGenerationModel = "MiniMax-H3-Image"
	tap(t, c, session, VerbBack)
	editor = tap(t, c, session, VerbField, "gm")
	if !strings.Contains(editor.Render.Built.Text, "Current: MiniMax-H3-Image") {
		t.Fatalf("the editor must show the stored model: %q", editor.Render.Built.Text)
	}
	if !strings.Contains(screenButtons(editor.Render.Built), "Clear model") {
		t.Fatalf("a stored model must be clearable: %q", screenButtons(editor.Render.Built))
	}

	tap(t, c, session, VerbClearGlobal, "gm")
	store.assertCalls(t, "SetGlobalField:image_generation_model=")
	if store.global.ImageGenerationModel != "" {
		t.Fatalf("expected the model to be cleared, got %q", store.global.ImageGenerationModel)
	}
}

func TestImageGenerationFieldCodesRoundTrip(t *testing.T) {
	for name, code := range map[string]string{
		"image_generation_enabled": "ig",
		"image_generation_model":   "gm",
	} {
		got, ok := FieldCode(name)
		if !ok || got != code {
			t.Fatalf("FieldCode(%q) = %q, %v; want %q", name, got, ok, code)
		}
		for _, global := range []bool{true, false} {
			if name == "image_generation_model" && !global {
				continue // global-only field
			}
			resolved, ok := FieldName(code, global)
			if !ok || resolved != name {
				t.Fatalf("FieldName(%q, global=%v) = %q, %v; want %q", code, global, resolved, ok, name)
			}
		}
	}
}
