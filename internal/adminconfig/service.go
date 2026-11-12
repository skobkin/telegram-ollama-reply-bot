package adminconfig

import (
	"context"
	"fmt"
	"sort"
	"text/template"
)

// BuiltinTemplates supplies the built-in prompt body per feature. The wiring
// layer provides the real constants (from internal/llm) so this package stays
// free of the llm import.
type BuiltinTemplates map[PromptFeature]string

// PromptValidator checks a prompt template body for a feature. The real
// implementation (execute-level validation from internal/llm) is injected at
// wiring time; a nil validator falls back to parse-only checks.
type PromptValidator func(feature PromptFeature, body string) error

type Service struct {
	store           Store
	builtins        BuiltinTemplates
	promptValidator PromptValidator
}

func NewService(store Store, builtins BuiltinTemplates, promptValidator PromptValidator) *Service {
	return &Service{store: store, builtins: builtins, promptValidator: promptValidator}
}

func (s *Service) Store() Store {
	return s.store
}

func (s *Service) GlobalFields() []string {
	return []string{"character_name", "language", "gender", "tone_mode", "allow_teasing", "default_interactivity_mode", "default_auto_summary_enabled", "show_reply_previews", "default_preview_mode", "keep_tool_history"}
}

func (s *Service) ChatFields() []string {
	return []string{"character_name", "language", "gender", "tone_mode", "allow_teasing", "interactivity_mode", "auto_summary_enabled", "preview_mode", "keep_tool_history"}
}

func (s *Service) PromptFeatures() []PromptFeature {
	return []PromptFeature{PromptFeatureChat, PromptFeatureSummarize, PromptFeatureImageRecognition}
}

func IsPromptFeature(feature PromptFeature) bool {
	switch feature {
	case PromptFeatureChat, PromptFeatureSummarize, PromptFeatureImageRecognition:
		return true
	default:
		return false
	}
}

func (s *Service) ResolveChatConfig(ctx context.Context, chatID int64) (ResolvedChatConfig, error) {
	global, err := s.store.GetGlobalSettings(ctx)
	if err != nil {
		return ResolvedChatConfig{}, err
	}

	previewMode := global.DefaultPreviewMode
	if previewMode == "" {
		previewMode = PreviewModeTools
	}

	resolved := ResolvedChatConfig{
		CharacterName:      global.CharacterName,
		Language:           global.Language,
		Gender:             global.Gender,
		ToneMode:           global.ToneMode,
		AllowTeasing:       global.AllowTeasing,
		InteractivityMode:  global.DefaultInteractivityMode,
		AutoSummaryEnabled: global.DefaultAutoSummaryEnabled,
		PreviewMode:        previewMode,
		KeepToolHistory:    global.KeepToolHistory,
	}

	if chatID == 0 {
		return resolved, nil
	}

	chat, ok, err := s.store.GetChatSettings(ctx, chatID)
	if err != nil {
		return ResolvedChatConfig{}, err
	}
	if !ok {
		if !global.ShowReplyPreviews {
			resolved.PreviewMode = PreviewModeOff
		}

		return resolved, nil
	}
	if chat.CharacterName != "" {
		resolved.CharacterName = chat.CharacterName
	}
	if chat.Language != "" {
		resolved.Language = chat.Language
	}
	if chat.Gender != "" {
		resolved.Gender = chat.Gender
	}
	if chat.ToneMode != "" {
		resolved.ToneMode = chat.ToneMode
	}
	if chat.AllowTeasing != nil {
		resolved.AllowTeasing = *chat.AllowTeasing
	}
	if chat.HasInteractivity && chat.InteractivityMode != "" {
		resolved.InteractivityMode = chat.InteractivityMode
	}
	if chat.AutoSummaryEnabled != nil {
		resolved.AutoSummaryEnabled = *chat.AutoSummaryEnabled
	}
	if chat.HasPreviewMode && chat.PreviewMode != "" {
		resolved.PreviewMode = chat.PreviewMode
	}
	if chat.KeepToolHistory != nil {
		resolved.KeepToolHistory = *chat.KeepToolHistory
	}

	// The master switch outranks every per-chat override: stored overrides
	// stay preserved but inert while previews are globally disabled.
	if !global.ShowReplyPreviews {
		resolved.PreviewMode = PreviewModeOff
	}

	return resolved, nil
}

func (s *Service) HasAdmins(adminIDs []int64) bool {
	return len(adminIDs) > 0
}

func (s *Service) ChatList(ctx context.Context) ([]ChatCatalogEntry, error) {
	chats, err := s.store.ListChats(ctx)
	if err != nil {
		return nil, err
	}
	sort.Slice(chats, func(i, j int) bool {
		if chats[i].LastSeenAt.Equal(chats[j].LastSeenAt) {
			return chats[i].ChatID < chats[j].ChatID
		}

		return chats[i].LastSeenAt.After(chats[j].LastSeenAt)
	})

	return chats, nil
}

func BoolPointer(v bool) *bool {
	return &v
}

func ValidatePrompt(feature PromptFeature, body string) error {
	if !IsPromptFeature(feature) {
		return fmt.Errorf("unknown prompt feature %q", feature)
	}

	if _, err := template.New(string(feature)).Parse(body); err != nil {
		return fmt.Errorf("parse template %s: %w", feature, err)
	}

	return nil
}
