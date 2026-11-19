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
func (s *promptStoreStub) GetAccessPolicy(context.Context) (adminconfig.AccessPolicy, error) {
	return adminconfig.AccessPolicyAllowlist, nil
}
func (s *promptStoreStub) SetAccessPolicy(context.Context, adminconfig.AccessPolicy, int64) error {
	return nil
}
func (s *promptStoreStub) ListWhitelistEntries(context.Context) ([]adminconfig.WhitelistEntry, error) {
	return nil, nil
}

func TestAdminPromptRendererUsesPerChatLanguageAndGender(t *testing.T) {
	svc := adminconfig.NewService(&promptStoreStub{
		global: adminconfig.GlobalSettings{
			CharacterName:            "bot",
			Language:                 "Russian",
			Gender:                   "neutral",
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
	}, nil, nil)

	renderer := NewAdminPromptRenderer(svc, true, "kitsune_bot")
	rendered, err := renderer.RenderChatSystemPrompt(context.Background(), PromptScope{ChatID: 5}, "gemma", "", "policy")
	if err != nil {
		t.Fatalf("render chat prompt: %v", err)
	}

	if !strings.Contains(rendered, "name=kitsune") || !strings.Contains(rendered, "lang=English") || !strings.Contains(rendered, "gender=female") {
		t.Fatalf("unexpected rendered prompt: %q", rendered)
	}
}

func TestAdminPromptRendererRendersBotUsername(t *testing.T) {
	svc := adminconfig.NewService(&promptStoreStub{
		global: adminconfig.GlobalSettings{
			CharacterName:            "bot",
			Language:                 "Russian",
			Gender:                   "neutral",
			DefaultInteractivityMode: adminconfig.InteractivityDisabled,
		},
		prompts: map[string]string{
			"chat:0": "handle=@{{.BotUsername}}",
		},
	}, nil, nil)

	rendered, err := NewAdminPromptRenderer(svc, true, "kitsune_bot").RenderChatSystemPrompt(context.Background(), PromptScope{}, "gemma", "", "")
	if err != nil {
		t.Fatalf("render chat prompt: %v", err)
	}
	if !strings.Contains(rendered, "handle=@kitsune_bot") {
		t.Fatalf("expected bot username in prompt, got %q", rendered)
	}

	empty, err := NewAdminPromptRenderer(svc, true, "").RenderChatSystemPrompt(context.Background(), PromptScope{}, "gemma", "", "")
	if err != nil {
		t.Fatalf("render chat prompt without username: %v", err)
	}
	if !strings.Contains(empty, "handle=@") {
		t.Fatalf("expected bare @ when username unset, got %q", empty)
	}
}

func TestDefaultChatPromptTemplateBotUsernameClause(t *testing.T) {
	svc := adminconfig.NewService(&promptStoreStub{
		global: adminconfig.GlobalSettings{
			CharacterName:            "bot",
			Language:                 "Russian",
			Gender:                   "neutral",
			DefaultInteractivityMode: adminconfig.InteractivityDisabled,
		},
	}, nil, nil)

	rendered, err := NewAdminPromptRenderer(svc, true, "kitsune_bot").RenderChatSystemPrompt(context.Background(), PromptScope{}, "gemma", "", "")
	if err != nil {
		t.Fatalf("render default chat prompt: %v", err)
	}
	if !strings.Contains(rendered, "Твой username в Telegram: @kitsune_bot.") {
		t.Fatalf("expected bot username clause in default prompt, got %q", rendered)
	}

	rendered, err = NewAdminPromptRenderer(svc, true, "").RenderChatSystemPrompt(context.Background(), PromptScope{}, "gemma", "", "")
	if err != nil {
		t.Fatalf("render default chat prompt without username: %v", err)
	}
	if strings.Contains(rendered, "Твой username в Telegram") {
		t.Fatalf("expected no bot username clause when unset, got %q", rendered)
	}
}

func TestAdminPromptRendererRendersToolPolicyInChatPrompt(t *testing.T) {
	svc := adminconfig.NewService(&promptStoreStub{
		global: adminconfig.GlobalSettings{
			CharacterName:            "bot",
			Language:                 "Russian",
			Gender:                   "neutral",
			DefaultInteractivityMode: adminconfig.InteractivityDisabled,
		},
		prompts: map[string]string{
			"chat:0": "policy={{.ToolPolicy}} lang={{.Language}} model={{.Model}}",
		},
	}, nil, nil)

	renderer := NewAdminPromptRenderer(svc, true, "")
	rendered, err := renderer.RenderChatSystemPrompt(context.Background(), PromptScope{}, "gemma", "ctx", "line one")
	if err != nil {
		t.Fatalf("render chat prompt: %v", err)
	}

	if !strings.Contains(rendered, "policy=line one") || !strings.Contains(rendered, "lang=Russian") || !strings.Contains(rendered, "model=gemma") {
		t.Fatalf("unexpected rendered prompt: %q", rendered)
	}
}

// TestAdminPromptRendererCapturedPersona pins the operation-snapshot
// rendering contract: a captured template body is executed with the
// captured settings and never re-resolved from the store, so a store
// mutation cannot split the persona mid-operation; an empty body falls back
// to the ordinary live resolution, and ChatPromptBody exposes the same body
// the snapshot captures.
func TestAdminPromptRendererCapturedPersona(t *testing.T) {
	store := &promptStoreStub{
		global: adminconfig.GlobalSettings{
			CharacterName:            "bot",
			Language:                 "Russian",
			Gender:                   "neutral",
			DefaultInteractivityMode: adminconfig.InteractivityDisabled,
		},
		prompts: map[string]string{
			"chat:5": "LIVE name={{.CharacterName}} lang={{.Language}}",
		},
	}
	renderer := NewAdminPromptRenderer(adminconfig.NewService(store, nil, nil), true, "")

	captured, err := renderer.RenderCapturedChatSystemPrompt(context.Background(), CapturedPersona{
		TemplateBody:  "CAPTURED name={{.CharacterName}} lang={{.Language}} model={{.Model}} policy={{.ToolPolicy}}",
		Language:      "English",
		Gender:        "female",
		CharacterName: "Ada",
	}, 5, "gemma", "hint", "no tools")
	if err != nil {
		t.Fatalf("render captured persona: %v", err)
	}
	if !strings.Contains(captured, "CAPTURED name=Ada lang=English model=gemma policy=no tools") {
		t.Fatalf("captured body must render with the snapshot settings, got %q", captured)
	}
	if strings.Contains(captured, "LIVE") {
		t.Fatalf("captured body must not re-resolve the live template, got %q", captured)
	}

	// A store change after the capture is invisible to the captured path.
	store.prompts["chat:5"] = "MUTATED name={{.CharacterName}}"
	again, err := renderer.RenderCapturedChatSystemPrompt(context.Background(), CapturedPersona{
		TemplateBody:  "CAPTURED name={{.CharacterName}}",
		Language:      "English",
		CharacterName: "Ada",
	}, 5, "gemma", "", "")
	if err != nil {
		t.Fatalf("render captured persona after store change: %v", err)
	}
	if !strings.Contains(again, "CAPTURED name=Ada") || strings.Contains(again, "MUTATED") {
		t.Fatalf("store mutation must not affect a captured body, got %q", again)
	}

	// Without a captured body the ordinary live resolution applies.
	live, err := renderer.RenderCapturedChatSystemPrompt(context.Background(), CapturedPersona{
		Language:      "English",
		CharacterName: "Ada",
	}, 5, "gemma", "", "")
	if err != nil {
		t.Fatalf("render live persona: %v", err)
	}
	if !strings.Contains(live, "MUTATED name=Ada") {
		t.Fatalf("empty captured body must fall back to the live template, got %q", live)
	}

	// ChatPromptBody resolves the same live body the fallback renders.
	body, err := renderer.ChatPromptBody(context.Background(), 5)
	if err != nil {
		t.Fatalf("chat prompt body: %v", err)
	}
	if body != "MUTATED name={{.CharacterName}}" {
		t.Fatalf("chat prompt body must be the live template body, got %q", body)
	}
}
