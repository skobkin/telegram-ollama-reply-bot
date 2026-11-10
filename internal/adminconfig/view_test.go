package adminconfig

import (
	"context"
	"testing"
)

func viewTestGlobal() GlobalSettings {
	return GlobalSettings{
		CharacterName:            "global-name",
		Language:                 "Russian",
		Gender:                   "neutral",
		ToneMode:                 "default",
		AllowTeasing:             false,
		DefaultInteractivityMode: InteractivityDisabled,
	}
}

func TestEffectiveGlobalConfigReturnsSettingsAndSpecs(t *testing.T) {
	svc := NewService(&stubStore{global: viewTestGlobal()})

	view, err := svc.EffectiveGlobalConfig(context.Background())
	if err != nil {
		t.Fatalf("effective global config: %v", err)
	}

	if view.Settings.CharacterName != "global-name" {
		t.Fatalf("unexpected settings: %+v", view.Settings)
	}
	if len(view.Fields) != len(GlobalFieldSpecs()) {
		t.Fatalf("expected %d specs, got %d", len(GlobalFieldSpecs()), len(view.Fields))
	}
}

func TestEffectiveChatConfigWithoutSettingsRowIsAllInherited(t *testing.T) {
	svc := NewService(&stubStore{global: viewTestGlobal()})

	view, err := svc.EffectiveChatConfig(context.Background(), -100123)
	if err != nil {
		t.Fatalf("effective chat config: %v", err)
	}

	if view.ChatID != -100123 {
		t.Fatalf("unexpected chat id: %d", view.ChatID)
	}
	for _, field := range view.Fields {
		if field.Source != SourceGlobal || field.Overridden {
			t.Fatalf("field %s should be inherited, got %+v", field.Spec.Name, field)
		}
		if field.Effective != field.GlobalValue {
			t.Fatalf("field %s effective %q should fall back to global %q", field.Spec.Name, field.Effective, field.GlobalValue)
		}
	}
}

func TestEffectiveChatConfigMarksOverridesExistenceBased(t *testing.T) {
	// language override equals the global value on purpose: it must still be
	// reported as an override.
	svc := NewService(&stubStore{
		global:    viewTestGlobal(),
		chatFound: true,
		chat: ChatSettings{
			CharacterName:     "kitsune",
			Language:          "Russian",
			AllowTeasing:      BoolPointer(true),
			InteractivityMode: InteractivityMentionsReplies,
			HasInteractivity:  true,
		},
	})

	view, err := svc.EffectiveChatConfig(context.Background(), 42)
	if err != nil {
		t.Fatalf("effective chat config: %v", err)
	}

	byName := map[string]EffectiveField{}
	for _, field := range view.Fields {
		byName[field.Spec.Name] = field
	}

	assertOverride(t, byName["character_name"], "kitsune")
	assertOverride(t, byName["language"], "Russian") // equal-valued override still flagged
	assertOverride(t, byName["allow_teasing"], "true")
	assertOverride(t, byName["interactivity_mode"], string(InteractivityMentionsReplies))

	if field := byName["gender"]; field.Overridden || field.Source != SourceGlobal {
		t.Fatalf("gender should be inherited, got %+v", field)
	}
	if field := byName["allow_teasing"]; field.GlobalValue != "false" {
		t.Fatalf("allow_teasing global value should be false, got %q", field.GlobalValue)
	}
}

func TestEffectiveChatConfigNilBoolAndUnsetInteractivityInherit(t *testing.T) {
	svc := NewService(&stubStore{
		global:    viewTestGlobal(),
		chatFound: true,
		chat:      ChatSettings{HasInteractivity: false},
	})

	view, err := svc.EffectiveChatConfig(context.Background(), 42)
	if err != nil {
		t.Fatalf("effective chat config: %v", err)
	}

	for _, field := range view.Fields {
		if field.Overridden {
			t.Fatalf("field %s should be inherited, got %+v", field.Spec.Name, field)
		}
	}

	byName := map[string]EffectiveField{}
	for _, field := range view.Fields {
		byName[field.Spec.Name] = field
	}
	if got := byName["allow_teasing"].Effective; got != "false" {
		t.Fatalf("allow_teasing effective should be global false, got %q", got)
	}
	if got := byName["interactivity_mode"].Effective; got != string(InteractivityDisabled) {
		t.Fatalf("interactivity effective should be global disabled, got %q", got)
	}
}

func TestEffectiveChatConfigReportsAllowlisted(t *testing.T) {
	svc := NewService(&stubStore{global: viewTestGlobal(), whitelistHit: true})

	view, err := svc.EffectiveChatConfig(context.Background(), 7)
	if err != nil {
		t.Fatalf("effective chat config: %v", err)
	}
	if !view.Allowlisted {
		t.Fatalf("expected allowlisted chat")
	}
}

func assertOverride(t *testing.T, field EffectiveField, want string) {
	t.Helper()

	if !field.Overridden || field.Source != SourceChatOverride {
		t.Fatalf("field %s should be a chat override, got %+v", field.Spec.Name, field)
	}
	if field.Effective != want {
		t.Fatalf("field %s effective should be %q, got %q", field.Spec.Name, want, field.Effective)
	}
}
