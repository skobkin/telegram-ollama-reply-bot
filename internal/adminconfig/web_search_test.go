package adminconfig

import (
	"context"
	"testing"
)

// TestResolveChatConfigWebSearch covers the inherit / enabled / disabled
// states against both global defaults, for chats with and without a settings
// row, and for the global-only (chatID 0) resolution.
func TestResolveChatConfigWebSearch(t *testing.T) {
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
		{"global on, chat disables", true, ChatSettings{WebSearchEnabled: BoolPointer(false)}, true, 1, false},
		{"global off, chat enables", false, ChatSettings{WebSearchEnabled: BoolPointer(true)}, true, 1, true},
		{"global on, chat explicitly enables", true, ChatSettings{WebSearchEnabled: BoolPointer(true)}, true, 1, true},
		{"global-only resolution ignores chat rows", true, ChatSettings{WebSearchEnabled: BoolPointer(false)}, true, 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			svc := NewService(&stubStore{
				global:    GlobalSettings{WebSearchEnabled: tt.global},
				chat:      tt.chat,
				chatFound: tt.chatFound,
			}, nil, nil)

			resolved, err := svc.ResolveChatConfig(context.Background(), tt.chatID)
			if err != nil {
				t.Fatalf("resolve chat config: %v", err)
			}
			if resolved.WebSearchEnabled != tt.want {
				t.Fatalf("web search enabled = %v, want %v", resolved.WebSearchEnabled, tt.want)
			}
		})
	}
}

func TestWebSearchFieldSpecs(t *testing.T) {
	t.Parallel()

	globalKinds := map[string]FieldKind{}
	for _, spec := range GlobalFieldSpecs() {
		globalKinds[spec.Name] = spec.Kind
	}
	if kind, ok := globalKinds["web_search_enabled"]; !ok || kind != KindBool {
		t.Fatalf("global web_search_enabled kind = %v (ok=%v), want KindBool", kind, ok)
	}

	chatKinds := map[string]FieldKind{}
	for _, spec := range ChatFieldSpecs() {
		chatKinds[spec.Name] = spec.Kind
	}
	if kind, ok := chatKinds["web_search_enabled"]; !ok || kind != KindBoolTriState {
		t.Fatalf("chat web_search_enabled kind = %v (ok=%v), want KindBoolTriState", kind, ok)
	}

	svc := NewService(&stubStore{}, nil, nil)
	if !containsString(svc.GlobalFields(), "web_search_enabled") || !containsString(svc.ChatFields(), "web_search_enabled") {
		t.Fatal("service field lists must include web_search_enabled")
	}
}

// The chat card must show whether web search is inherited or overridden, and
// the global value it would inherit.
func TestEffectiveChatConfigWebSearchSource(t *testing.T) {
	t.Parallel()

	find := func(cfg EffectiveChatConfig) EffectiveField {
		for _, field := range cfg.Fields {
			if field.Spec.Name == "web_search_enabled" {
				return field
			}
		}
		t.Fatal("web_search_enabled missing from the effective chat config")

		return EffectiveField{}
	}

	inherited, err := NewService(&stubStore{global: GlobalSettings{WebSearchEnabled: true}}, nil, nil).
		EffectiveChatConfig(context.Background(), 1)
	if err != nil {
		t.Fatalf("effective chat config: %v", err)
	}
	if field := find(inherited); field.Overridden || field.Source != SourceGlobal || field.GlobalValue != "true" {
		t.Fatalf("inherited field = %+v", field)
	}

	overridden, err := NewService(&stubStore{
		global:    GlobalSettings{WebSearchEnabled: true},
		chat:      ChatSettings{WebSearchEnabled: BoolPointer(false)},
		chatFound: true,
	}, nil, nil).EffectiveChatConfig(context.Background(), 1)
	if err != nil {
		t.Fatalf("effective chat config: %v", err)
	}
	if field := find(overridden); !field.Overridden || field.Source != SourceChatOverride || field.Effective != "false" || field.GlobalValue != "true" {
		t.Fatalf("overridden field = %+v", field)
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}

	return false
}
