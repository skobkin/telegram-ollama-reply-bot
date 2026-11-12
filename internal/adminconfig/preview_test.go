package adminconfig

import (
	"context"
	"testing"
)

// TestResolveChatConfigPreviewSettings covers the preview resolution matrix:
// master switch dominance, chat overrides, inheritance defaults, and the
// keep-tool-history tri-state.
func TestResolveChatConfigPreviewSettings(t *testing.T) {
	masterOn := GlobalSettings{
		ShowReplyPreviews:  true,
		DefaultPreviewMode: PreviewModeTools,
		KeepToolHistory:    true,
	}

	tests := []struct {
		name            string
		global          GlobalSettings
		chat            ChatSettings
		chatFound       bool
		wantMode        PreviewMode
		wantKeepHistory bool
	}{
		{
			name:            "master off forces off despite chat override",
			global:          GlobalSettings{ShowReplyPreviews: false, DefaultPreviewMode: PreviewModeAllTools},
			chat:            ChatSettings{PreviewMode: PreviewModeAllTools, HasPreviewMode: true, KeepToolHistory: BoolPointer(true)},
			chatFound:       true,
			wantMode:        PreviewModeOff,
			wantKeepHistory: true,
		},
		{
			name:            "master on inherits global default without chat row",
			global:          masterOn,
			chatFound:       false,
			wantMode:        PreviewModeTools,
			wantKeepHistory: true,
		},
		{
			name:            "master on inherits global default with inherit chat row",
			global:          masterOn,
			chat:            ChatSettings{},
			chatFound:       true,
			wantMode:        PreviewModeTools,
			wantKeepHistory: true,
		},
		{
			name:            "chat override wins over global default",
			global:          masterOn,
			chat:            ChatSettings{PreviewMode: PreviewModeOff, HasPreviewMode: true},
			chatFound:       true,
			wantMode:        PreviewModeOff,
			wantKeepHistory: true,
		},
		{
			name:            "chat override can raise the mode",
			global:          masterOn,
			chat:            ChatSettings{PreviewMode: PreviewModeAllTools, HasPreviewMode: true},
			chatFound:       true,
			wantMode:        PreviewModeAllTools,
			wantKeepHistory: true,
		},
		{
			name:            "empty global default falls back to tools",
			global:          GlobalSettings{ShowReplyPreviews: true},
			chatFound:       false,
			wantMode:        PreviewModeTools,
			wantKeepHistory: false,
		},
		{
			name:            "chat keep-tool-history override wins",
			global:          masterOn,
			chat:            ChatSettings{KeepToolHistory: BoolPointer(false)},
			chatFound:       true,
			wantMode:        PreviewModeTools,
			wantKeepHistory: false,
		},
		{
			name:            "global keep-tool-history off inherits to chat",
			global:          GlobalSettings{ShowReplyPreviews: true, DefaultPreviewMode: PreviewModeAllTools, KeepToolHistory: false},
			chat:            ChatSettings{},
			chatFound:       true,
			wantMode:        PreviewModeAllTools,
			wantKeepHistory: false,
		},
		{
			name:            "chatID zero resolves globals only",
			global:          GlobalSettings{ShowReplyPreviews: true, DefaultPreviewMode: PreviewModeAllTools, KeepToolHistory: true},
			chat:            ChatSettings{PreviewMode: PreviewModeOff, HasPreviewMode: true},
			chatFound:       true,
			wantMode:        PreviewModeAllTools,
			wantKeepHistory: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := NewService(&stubStore{global: tt.global, chat: tt.chat, chatFound: tt.chatFound}, nil, nil)

			chatID := int64(1)
			if tt.name == "chatID zero resolves globals only" {
				chatID = 0
			}

			resolved, err := svc.ResolveChatConfig(context.Background(), chatID)
			if err != nil {
				t.Fatalf("resolve chat config: %v", err)
			}
			if resolved.PreviewMode != tt.wantMode {
				t.Fatalf("preview mode = %q, want %q", resolved.PreviewMode, tt.wantMode)
			}
			if resolved.KeepToolHistory != tt.wantKeepHistory {
				t.Fatalf("keep tool history = %v, want %v", resolved.KeepToolHistory, tt.wantKeepHistory)
			}
		})
	}
}

// TestPreviewModeValidation pins the mode sets: per-chat includes off, the
// global default does not (the master switch governs that).
func TestPreviewModeValidation(t *testing.T) {
	if !IsValidPreviewMode(PreviewModeOff) || !IsValidPreviewMode(PreviewModeTools) || !IsValidPreviewMode(PreviewModeAllTools) {
		t.Fatal("chat modes must include off, tools, all_tools")
	}
	if IsValidPreviewMode("bogus") {
		t.Fatal("bogus chat mode accepted")
	}

	if !IsValidGlobalPreviewMode(PreviewModeTools) || !IsValidGlobalPreviewMode(PreviewModeAllTools) {
		t.Fatal("global modes must include tools, all_tools")
	}
	if IsValidGlobalPreviewMode(PreviewModeOff) {
		t.Fatal("global mode must not include off; the master switch governs that")
	}
}

// TestPreviewFieldSpecs pins the field kinds so the UI and storage layers
// stay in sync with the domain.
func TestPreviewFieldSpecs(t *testing.T) {
	specs := map[string]FieldSpec{}
	for _, spec := range GlobalFieldSpecs() {
		specs[spec.Name] = spec
	}

	if spec, ok := specs["show_reply_previews"]; !ok || spec.Kind != KindBool {
		t.Fatalf("show_reply_previews spec: %+v (ok=%v)", spec, ok)
	}
	if spec, ok := specs["default_preview_mode"]; !ok || spec.Kind != KindEnum || len(spec.Enum) != 2 {
		t.Fatalf("default_preview_mode spec: %+v (ok=%v)", spec, ok)
	}
	if spec, ok := specs["keep_tool_history"]; !ok || spec.Kind != KindBool {
		t.Fatalf("keep_tool_history spec: %+v (ok=%v)", spec, ok)
	}

	chatSpecs := map[string]FieldSpec{}
	for _, spec := range ChatFieldSpecs() {
		chatSpecs[spec.Name] = spec
	}
	if spec, ok := chatSpecs["preview_mode"]; !ok || spec.Kind != KindEnum || len(spec.Enum) != 3 {
		t.Fatalf("preview_mode spec: %+v (ok=%v)", spec, ok)
	}
	if spec, ok := chatSpecs["keep_tool_history"]; !ok || spec.Kind != KindBoolTriState {
		t.Fatalf("chat keep_tool_history spec: %+v (ok=%v)", spec, ok)
	}

	// Chat enum accepts off; inherit is intentionally absent (clear-override).
	chatPreview := chatSpecs["preview_mode"]
	if _, err := chatPreview.ParseAndValidate("off"); err != nil {
		t.Fatalf("chat preview_mode rejected off: %v", err)
	}
	if _, err := chatPreview.ParseAndValidate("inherit"); err == nil {
		t.Fatal("chat preview_mode must not accept inherit via value parse")
	}
}
