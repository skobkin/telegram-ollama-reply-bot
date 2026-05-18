package adminconfig

import (
	"context"
	"fmt"
	"sort"
	"text/template"
)

type Service struct {
	store Store
}

func NewService(store Store) *Service {
	return &Service{store: store}
}

func (s *Service) Store() Store {
	return s.store
}

func (s *Service) GlobalFields() []string {
	return []string{"character_name", "language", "gender", "tone_mode", "allow_teasing", "default_interactivity_mode"}
}

func (s *Service) ChatFields() []string {
	return []string{"alias", "language", "gender", "tone_mode", "allow_teasing", "interactivity_mode"}
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

	resolved := ResolvedChatConfig{
		CharacterName:     global.CharacterName,
		Language:          global.Language,
		Gender:            global.Gender,
		ToneMode:          global.ToneMode,
		AllowTeasing:      global.AllowTeasing,
		InteractivityMode: global.DefaultInteractivityMode,
	}

	if chatID == 0 {
		return resolved, nil
	}

	chat, ok, err := s.store.GetChatSettings(ctx, chatID)
	if err != nil {
		return ResolvedChatConfig{}, err
	}
	if !ok {
		return resolved, nil
	}
	if chat.Alias != "" {
		resolved.Alias = chat.Alias
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

func (s *Service) ShouldAllowChat(ctx context.Context, chatID int64, isAdminDM bool) (bool, error) {
	if isAdminDM {
		return true, nil
	}

	chats, err := s.store.ListWhitelistChats(ctx)
	if err != nil {
		return false, err
	}
	if len(chats) == 0 {
		return true, nil
	}

	return s.store.IsChatWhitelisted(ctx, chatID)
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
