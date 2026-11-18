package app

import (
	"context"
	"testing"

	"telegram-ollama-reply-bot/internal/adminconfig"
	"telegram-ollama-reply-bot/internal/llm"
)

// settingsStubStore backs the modelResolver tests: only the settings reads
// are meaningful; the rest of the Store surface stays inert.
type settingsStubStore struct {
	adminconfig.Store

	global adminconfig.GlobalSettings
	chat   adminconfig.ChatSettings
	found  bool
}

func (s *settingsStubStore) GetGlobalSettings(context.Context) (adminconfig.GlobalSettings, error) {
	return s.global, nil
}

func (s *settingsStubStore) GetChatSettings(context.Context, int64) (adminconfig.ChatSettings, bool, error) {
	return s.chat, s.found, nil
}

// TestModelResolverFeatureMapping pins the feature-to-setting mapping of the
// adminconfig-backed resolver: chat is per-chat scoped, summarize and image
// recognition are global-only, and unknown features resolve as unset instead
// of erroring.
func TestModelResolverFeatureMapping(t *testing.T) {
	const chatID = int64(555)

	store := &settingsStubStore{
		global: adminconfig.GlobalSettings{
			ChatModel:             "global-chat",
			SummarizeModel:        "global-summary",
			ImageRecognitionModel: "global-vision",
		},
		chat: adminconfig.ChatSettings{
			ChatID:    chatID,
			ChatModel: "chat-override",
		},
		found: true,
	}

	resolver := modelResolver{admin: adminconfig.NewService(store, nil, nil)}
	ctx := context.Background()

	chatModel, err := resolver.ResolveModel(ctx, llm.FeatureChat, chatID)
	if err != nil {
		t.Fatalf("resolve chat: %v", err)
	}
	if chatModel != "chat-override" {
		t.Fatalf("expected the chat override to win, got %q", chatModel)
	}

	// Global-only features: the same values regardless of the chat scoping.
	for _, chatScope := range []int64{0, chatID} {
		summaryModel, err := resolver.ResolveModel(ctx, llm.FeatureSummarize, chatScope)
		if err != nil {
			t.Fatalf("resolve summarize: %v", err)
		}
		if summaryModel != "global-summary" {
			t.Fatalf("expected the global summarize model, got %q", summaryModel)
		}

		visionModel, err := resolver.ResolveModel(ctx, llm.FeatureImageRecognition, chatScope)
		if err != nil {
			t.Fatalf("resolve image recognition: %v", err)
		}
		if visionModel != "global-vision" {
			t.Fatalf("expected the global image model, got %q", visionModel)
		}
	}

	// Unknown features have no runtime override.
	unknown, err := resolver.ResolveModel(ctx, llm.Feature("decision"), 0)
	if err != nil {
		t.Fatalf("resolve unknown feature: %v", err)
	}
	if unknown != "" {
		t.Fatalf("expected unset for an unknown feature, got %q", unknown)
	}
}

// TestModelResolverUnsetChatOverrideFallsBackToGlobal pins the fallback of
// the chat scope to the global default when no chat row exists.
func TestModelResolverUnsetChatOverrideFallsBackToGlobal(t *testing.T) {
	store := &settingsStubStore{
		global: adminconfig.GlobalSettings{ChatModel: "global-chat"},
	}

	resolver := modelResolver{admin: adminconfig.NewService(store, nil, nil)}

	model, err := resolver.ResolveModel(context.Background(), llm.FeatureChat, 123)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if model != "global-chat" {
		t.Fatalf("expected the global default, got %q", model)
	}
}
