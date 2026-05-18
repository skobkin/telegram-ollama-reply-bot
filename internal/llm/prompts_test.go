package llm

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"telegram-ollama-reply-bot/internal/adminconfig"
)

type promptStoreStub struct {
	global    adminconfig.GlobalSettings
	chat      adminconfig.ChatSettings
	chatFound bool
	prompts   map[string]string
}

func (s *promptStoreStub) GetGlobalSettings(context.Context) (adminconfig.GlobalSettings, error) {
	return s.global, nil
}
func (s *promptStoreStub) SetGlobalField(context.Context, string, string, int64) error { return nil }
func (s *promptStoreStub) GetChatSettings(context.Context, int64) (adminconfig.ChatSettings, bool, error) {
	return s.chat, s.chatFound, nil
}
func (s *promptStoreStub) SetChatField(context.Context, int64, string, string, int64) error {
	return nil
}
func (s *promptStoreStub) ClearChatField(context.Context, int64, string, int64) error { return nil }
func (s *promptStoreStub) GetPromptTemplate(_ context.Context, feature adminconfig.PromptFeature, chatID int64) (string, bool, error) {
	body, ok := s.prompts[fmt.Sprintf("%s:%d", feature, chatID)]

	return body, ok, nil
}
func (s *promptStoreStub) SetPromptTemplate(context.Context, adminconfig.PromptFeature, int64, string, int64) error {
	return nil
}
func (s *promptStoreStub) ClearPromptTemplate(context.Context, adminconfig.PromptFeature, int64) error {
	return nil
}
func (s *promptStoreStub) UpsertChatCatalog(context.Context, adminconfig.ChatCatalogEntry) error {
	return nil
}
func (s *promptStoreStub) ListChats(context.Context) ([]adminconfig.ChatCatalogEntry, error) {
	return nil, nil
}
func (s *promptStoreStub) AddWhitelistChat(context.Context, int64, int64) error   { return nil }
func (s *promptStoreStub) RemoveWhitelistChat(context.Context, int64) error       { return nil }
func (s *promptStoreStub) ListWhitelistChats(context.Context) ([]int64, error)    { return nil, nil }
func (s *promptStoreStub) IsChatWhitelisted(context.Context, int64) (bool, error) { return false, nil }

func TestAdminPromptRendererUsesPerChatLanguageAndGender(t *testing.T) {
	svc := adminconfig.NewService(&promptStoreStub{
		global: adminconfig.GlobalSettings{
			CharacterName:            "bot",
			Language:                 "Russian",
			Gender:                   "neutral",
			ToneMode:                 "default",
			DefaultInteractivityMode: adminconfig.InteractivityDisabled,
		},
		chatFound: true,
		chat: adminconfig.ChatSettings{
			CharacterName: "kitsune",
			Language:      "English",
			Gender:        "female",
		},
		prompts: map[string]string{
			"chat:5": "name={{.CharacterName}} lang={{.Language}} gender={{.Gender}} model={{.Model}}",
		},
	})

	renderer := NewAdminPromptRenderer(svc)
	rendered, err := renderer.RenderChatSystemPrompt(context.Background(), PromptScope{ChatID: 5}, "gemma", "", "policy")
	if err != nil {
		t.Fatalf("render chat prompt: %v", err)
	}

	if !strings.Contains(rendered, "name=kitsune") || !strings.Contains(rendered, "lang=English") || !strings.Contains(rendered, "gender=female") {
		t.Fatalf("unexpected rendered prompt: %q", rendered)
	}
}

func TestAdminPromptRendererRendersToolPolicyInChatPrompt(t *testing.T) {
	svc := adminconfig.NewService(&promptStoreStub{
		global: adminconfig.GlobalSettings{
			CharacterName:            "bot",
			Language:                 "Russian",
			Gender:                   "neutral",
			ToneMode:                 "default",
			DefaultInteractivityMode: adminconfig.InteractivityDisabled,
		},
		prompts: map[string]string{
			"chat:0": "policy={{.ToolPolicy}} lang={{.Language}} model={{.Model}}",
		},
	})

	renderer := NewAdminPromptRenderer(svc)
	rendered, err := renderer.RenderChatSystemPrompt(context.Background(), PromptScope{}, "gemma", "ctx", "line one")
	if err != nil {
		t.Fatalf("render chat prompt: %v", err)
	}

	if !strings.Contains(rendered, "policy=line one") || !strings.Contains(rendered, "lang=Russian") || !strings.Contains(rendered, "model=gemma") {
		t.Fatalf("unexpected rendered prompt: %q", rendered)
	}
}
