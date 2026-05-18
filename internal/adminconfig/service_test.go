package adminconfig

import (
	"context"
	"fmt"
	"testing"
)

type stubStore struct {
	global       GlobalSettings
	chat         ChatSettings
	chatFound    bool
	prompts      map[string]string
	whitelist    []int64
	whitelistHit bool
}

func (s *stubStore) GetGlobalSettings(context.Context) (GlobalSettings, error) { return s.global, nil }
func (s *stubStore) SetGlobalField(context.Context, string, string, int64) error {
	return nil
}
func (s *stubStore) GetChatSettings(context.Context, int64) (ChatSettings, bool, error) {
	return s.chat, s.chatFound, nil
}
func (s *stubStore) SetChatField(context.Context, int64, string, string, int64) error { return nil }
func (s *stubStore) ClearChatField(context.Context, int64, string, int64) error       { return nil }
func (s *stubStore) GetPromptTemplate(_ context.Context, feature PromptFeature, chatID int64) (string, bool, error) {
	key := fmt.Sprintf("%s:%d", feature, chatID)
	body, ok := s.prompts[key]

	return body, ok, nil
}
func (s *stubStore) SetPromptTemplate(context.Context, PromptFeature, int64, string, int64) error {
	return nil
}
func (s *stubStore) ClearPromptTemplate(context.Context, PromptFeature, int64) error { return nil }
func (s *stubStore) UpsertChatCatalog(context.Context, ChatCatalogEntry) error       { return nil }
func (s *stubStore) ListChats(context.Context) ([]ChatCatalogEntry, error)           { return nil, nil }
func (s *stubStore) AddWhitelistChat(context.Context, int64, int64) error            { return nil }
func (s *stubStore) RemoveWhitelistChat(context.Context, int64) error                { return nil }
func (s *stubStore) ListWhitelistChats(context.Context) ([]int64, error)             { return s.whitelist, nil }
func (s *stubStore) IsChatWhitelisted(context.Context, int64) (bool, error) {
	return s.whitelistHit, nil
}

func TestResolveChatConfigUsesChatOverrides(t *testing.T) {
	svc := NewService(&stubStore{
		global: GlobalSettings{
			CharacterName:            "global",
			Language:                 "Russian",
			Gender:                   "neutral",
			ToneMode:                 "default",
			AllowTeasing:             false,
			DefaultInteractivityMode: InteractivityDisabled,
		},
		chatFound: true,
		chat: ChatSettings{
			CharacterName:     "kitsune",
			Language:          "English",
			Gender:            "female",
			ToneMode:          "chaotic",
			AllowTeasing:      BoolPointer(true),
			InteractivityMode: InteractivityMentionsReplies,
			HasInteractivity:  true,
		},
	})

	resolved, err := svc.ResolveChatConfig(context.Background(), 1)
	if err != nil {
		t.Fatalf("resolve chat config: %v", err)
	}

	if resolved.CharacterName != "kitsune" || resolved.Language != "English" || resolved.Gender != "female" || resolved.ToneMode != "chaotic" || !resolved.AllowTeasing || resolved.InteractivityMode != InteractivityMentionsReplies {
		t.Fatalf("unexpected resolved config: %+v", resolved)
	}
}

func TestResolveChatConfigFallsBackToGlobalCharacterName(t *testing.T) {
	svc := NewService(&stubStore{
		global: GlobalSettings{
			CharacterName:            "global",
			Language:                 "Russian",
			Gender:                   "neutral",
			ToneMode:                 "default",
			AllowTeasing:             false,
			DefaultInteractivityMode: InteractivityDisabled,
		},
		chatFound: true,
		chat: ChatSettings{
			Language: "English",
		},
	})

	resolved, err := svc.ResolveChatConfig(context.Background(), 1)
	if err != nil {
		t.Fatalf("resolve chat config: %v", err)
	}

	if resolved.CharacterName != "global" {
		t.Fatalf("expected global character name fallback, got %q", resolved.CharacterName)
	}
}

func TestShouldAllowChatBypassesWhitelistForAdminDM(t *testing.T) {
	svc := NewService(&stubStore{whitelist: []int64{1}, whitelistHit: false})

	allowed, err := svc.ShouldAllowChat(context.Background(), 123, true)
	if err != nil {
		t.Fatalf("should allow chat: %v", err)
	}
	if !allowed {
		t.Fatalf("expected admin DM to bypass whitelist")
	}
}

func TestPromptFeaturesExcludesToolUse(t *testing.T) {
	svc := NewService(&stubStore{})

	features := svc.PromptFeatures()
	expected := []PromptFeature{PromptFeatureChat, PromptFeatureSummarize, PromptFeatureImageRecognition}
	if fmt.Sprint(features) != fmt.Sprint(expected) {
		t.Fatalf("unexpected prompt features: got %v want %v", features, expected)
	}
}
