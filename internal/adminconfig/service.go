package adminconfig

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"text/template"
	"time"

	"telegram-ollama-reply-bot/internal/llm"
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
	return []string{"alias", "tone_mode", "allow_teasing", "interactivity_mode"}
}

func (s *Service) PromptFeatures() []PromptFeature {
	return []PromptFeature{PromptFeatureChat, PromptFeatureSummarize, PromptFeatureImageRecognition}
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

func (s *Service) RenderChatPrompt(ctx context.Context, scope llm.PromptScope, model, compactContext string) (string, error) {
	resolved, err := s.ResolveChatConfig(ctx, scope.ChatID)
	if err != nil {
		return "", err
	}

	body, err := s.promptBody(ctx, PromptFeatureChat, scope.ChatID, llm.DefaultChatPromptTemplate)
	if err != nil {
		return "", err
	}

	return executeTemplate("chat", body, struct {
		Language      string
		Model         string
		Context       string
		Gender        string
		CharacterName string
		ToneMode      string
		AllowTeasing  bool
	}{
		Language:      resolved.Language,
		Model:         model,
		Context:       compactContext,
		Gender:        resolved.Gender,
		CharacterName: resolved.CharacterName,
		ToneMode:      resolved.ToneMode,
		AllowTeasing:  resolved.AllowTeasing,
	})
}

func (s *Service) RenderSummarizePrompt(ctx context.Context, scope llm.PromptScope) (string, error) {
	resolved, err := s.ResolveChatConfig(ctx, scope.ChatID)
	if err != nil {
		return "", err
	}

	body, err := s.promptBody(ctx, PromptFeatureSummarize, scope.ChatID, llm.DefaultSummarizePromptTemplate)
	if err != nil {
		return "", err
	}

	return executeTemplate("summarize", body, struct {
		Language  string
		MaxLength int
	}{
		Language:  resolved.Language,
		MaxLength: llm.DefaultMaxSummaryLength,
	})
}

func (s *Service) RenderImageRecognitionPrompt(ctx context.Context, scope llm.PromptScope) (string, error) {
	resolved, err := s.ResolveChatConfig(ctx, scope.ChatID)
	if err != nil {
		return "", err
	}

	body, err := s.promptBody(ctx, PromptFeatureImageRecognition, scope.ChatID, llm.DefaultImageRecognitionPromptTemplate)
	if err != nil {
		return "", err
	}

	return executeTemplate("image_recognition", body, struct {
		Language string
	}{
		Language: resolved.Language,
	})
}

func (s *Service) promptBody(ctx context.Context, feature PromptFeature, chatID int64, fallback string) (string, error) {
	body, ok, err := s.store.GetPromptTemplate(ctx, feature, chatID)
	if err != nil {
		return "", err
	}
	if ok {
		return body, nil
	}
	if chatID != 0 {
		body, ok, err = s.store.GetPromptTemplate(ctx, feature, 0)
		if err != nil {
			return "", err
		}
		if ok {
			return body, nil
		}
	}

	return fallback, nil
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

func executeTemplate(name, body string, data any) (string, error) {
	tmpl, err := template.New(name).Parse(body)
	if err != nil {
		return "", err
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", err
	}

	return buf.String(), nil
}

func BoolPointer(v bool) *bool {
	return &v
}

func NowUTC() time.Time {
	return time.Now().UTC()
}

func ValidatePrompt(feature PromptFeature, body string) error {
	return llm.ValidatePromptTemplate(string(feature), body)
}

func FieldValueSummary(field, value string) string {
	return fmt.Sprintf("%s=%s", field, value)
}
