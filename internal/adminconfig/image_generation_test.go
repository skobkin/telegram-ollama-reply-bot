package adminconfig

import (
	"context"
	"testing"
)

// TestResolveChatConfigImageGeneration covers the inherit / enabled / disabled
// states against both global defaults, with and without a chat settings row,
// and for the global-only (chatID 0) resolution.
func TestResolveChatConfigImageGeneration(t *testing.T) {
	tests := []struct {
		name      string
		global    bool
		chat      ChatSettings
		chatFound bool
		chatID    int64
		want      bool
	}{
		{"global on, no chat row", true, ChatSettings{}, false, 1, true},
		{"global off, no chat row", false, ChatSettings{}, false, 1, false},
		{"global on, chat inherits", true, ChatSettings{}, true, 1, true},
		{"global off, chat inherits", false, ChatSettings{}, true, 1, false},
		{"global on, chat disables", true, ChatSettings{ImageGenerationEnabled: BoolPointer(false)}, true, 1, false},
		{"global off, chat enables", false, ChatSettings{ImageGenerationEnabled: BoolPointer(true)}, true, 1, true},
		{"global-only resolution ignores chat rows", true, ChatSettings{ImageGenerationEnabled: BoolPointer(false)}, true, 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			svc := NewService(&stubStore{
				global:    GlobalSettings{ImageGenerationEnabled: tt.global},
				chat:      tt.chat,
				chatFound: tt.chatFound,
			}, nil, nil)

			resolved, err := svc.ResolveChatConfig(context.Background(), tt.chatID)
			if err != nil {
				t.Fatalf("resolve chat config: %v", err)
			}
			if resolved.ImageGenerationEnabled != tt.want {
				t.Fatalf("image generation enabled = %v, want %v", resolved.ImageGenerationEnabled, tt.want)
			}
		})
	}
}

// The generation model is global-only: every chat resolves the global value,
// and an unset model stays empty — there is no environment fallback (#139).
func TestResolveChatConfigImageGenerationModelIsGlobalOnly(t *testing.T) {
	t.Parallel()

	svc := NewService(&stubStore{
		global:    GlobalSettings{ImageGenerationModel: "MiniMax-H3-Image"},
		chat:      ChatSettings{ChatModel: "some-chat-model"},
		chatFound: true,
	}, nil, nil)

	resolved, err := svc.ResolveChatConfig(context.Background(), 1)
	if err != nil {
		t.Fatalf("resolve chat config: %v", err)
	}
	if resolved.ImageGenerationModel != "MiniMax-H3-Image" {
		t.Fatalf("image generation model = %q", resolved.ImageGenerationModel)
	}

	unset, err := NewService(&stubStore{}, nil, nil).ResolveChatConfig(context.Background(), 1)
	if err != nil {
		t.Fatalf("resolve unset chat config: %v", err)
	}
	if unset.ImageGenerationModel != "" {
		t.Fatalf("an unset model must stay empty, got %q", unset.ImageGenerationModel)
	}
}

func TestImageGenerationFieldSpecs(t *testing.T) {
	t.Parallel()

	global := map[string]FieldSpec{}
	for _, spec := range GlobalFieldSpecs() {
		global[spec.Name] = spec
	}
	if spec, ok := global["image_generation_enabled"]; !ok || spec.Kind != KindBool {
		t.Fatalf("global image_generation_enabled = %+v (ok=%v), want KindBool", spec, ok)
	}
	model, ok := global["image_generation_model"]
	if !ok || model.Kind != KindString || !model.Clearable || model.MaxLen != 128 {
		t.Fatalf("global image_generation_model = %+v (ok=%v), want a clearable 128-byte string", model, ok)
	}

	chat := map[string]FieldSpec{}
	for _, spec := range ChatFieldSpecs() {
		chat[spec.Name] = spec
	}
	if spec, ok := chat["image_generation_enabled"]; !ok || spec.Kind != KindBoolTriState {
		t.Fatalf("chat image_generation_enabled = %+v (ok=%v), want KindBoolTriState", spec, ok)
	}
	if _, present := chat["image_generation_model"]; present {
		t.Fatal("image_generation_model must not be a per-chat field")
	}

	svc := NewService(&stubStore{}, nil, nil)
	for _, name := range []string{"image_generation_enabled", "image_generation_model"} {
		if !containsString(svc.GlobalFields(), name) {
			t.Fatalf("global field list must include %s", name)
		}
	}
	if !containsString(svc.ChatFields(), "image_generation_enabled") || containsString(svc.ChatFields(), "image_generation_model") {
		t.Fatal("chat field list must include image_generation_enabled and exclude the global-only model")
	}
}

// The chat card must show whether image generation is inherited or
// overridden, and the global value it would inherit.
func TestEffectiveChatConfigImageGenerationSource(t *testing.T) {
	t.Parallel()

	find := func(cfg EffectiveChatConfig) EffectiveField {
		for _, field := range cfg.Fields {
			if field.Spec.Name == "image_generation_enabled" {
				return field
			}
		}
		t.Fatal("image_generation_enabled missing from the effective chat config")

		return EffectiveField{}
	}

	inherited, err := NewService(&stubStore{global: GlobalSettings{ImageGenerationEnabled: true}}, nil, nil).
		EffectiveChatConfig(context.Background(), 1)
	if err != nil {
		t.Fatalf("effective chat config: %v", err)
	}
	if field := find(inherited); field.Overridden || field.Source != SourceGlobal || field.GlobalValue != "true" {
		t.Fatalf("inherited field = %+v", field)
	}

	overridden, err := NewService(&stubStore{
		global:    GlobalSettings{ImageGenerationEnabled: true},
		chat:      ChatSettings{ImageGenerationEnabled: BoolPointer(false)},
		chatFound: true,
	}, nil, nil).EffectiveChatConfig(context.Background(), 1)
	if err != nil {
		t.Fatalf("effective chat config: %v", err)
	}
	if field := find(overridden); !field.Overridden || field.Source != SourceChatOverride || field.Effective != "false" || field.GlobalValue != "true" {
		t.Fatalf("overridden field = %+v", field)
	}
}
