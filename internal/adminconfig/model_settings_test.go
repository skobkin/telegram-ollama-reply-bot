package adminconfig

import (
	"context"
	"testing"
)

func TestResolveChatConfigModelAndProfileResolution(t *testing.T) {
	chatID := int64(555)

	t.Run("empty settings resolve to unset and non-interactive", func(t *testing.T) {
		store := &stubStore{chatFound: true}
		svc := NewService(store, nil, nil)

		resolved, err := svc.ResolveChatConfig(context.Background(), chatID)
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if resolved.ChatModel != "" || resolved.SystemOneModel != "" || resolved.DecisionInstructions != "" {
			t.Fatalf("expected unset models and instructions, got %+v", resolved)
		}
		if resolved.InteractionProfile != ProfileNonInteractive {
			t.Fatalf("expected non_interactive default, got %q", resolved.InteractionProfile)
		}
	})

	t.Run("global defaults apply without chat overrides", func(t *testing.T) {
		store := &stubStore{
			global: GlobalSettings{
				ChatModel:                 "gemma4:e4b",
				SummarizeModel:            "summarizer-8b",
				ImageRecognitionModel:     "vision-mini",
				SystemOneModel:            "laya",
				DefaultInteractionProfile: ProfileTalkLow,
				DecisionInstructions:      "Be curious.",
			},
		}
		svc := NewService(store, nil, nil)

		resolved, err := svc.ResolveChatConfig(context.Background(), chatID)
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if resolved.ChatModel != "gemma4:e4b" || resolved.SystemOneModel != "laya" {
			t.Fatalf("unexpected models: %+v", resolved)
		}
		if resolved.InteractionProfile != ProfileTalkLow || resolved.DecisionInstructions != "Be curious." {
			t.Fatalf("unexpected profile/instructions: %+v", resolved)
		}
	})

	t.Run("chat overrides win over global defaults", func(t *testing.T) {
		store := &stubStore{
			global: GlobalSettings{
				ChatModel:                 "gemma4:e4b",
				SystemOneModel:            "laya",
				DefaultInteractionProfile: ProfileTalkLow,
				DecisionInstructions:      "Be curious.",
			},
			chat: ChatSettings{
				ChatID:               chatID,
				ChatModel:            "chat-override-8b",
				SystemOneModel:       "julia-1",
				InteractionProfile:   ProfileReactionsHigh,
				DecisionInstructions: "Never discuss politics.",
			},
			chatFound: true,
		}
		svc := NewService(store, nil, nil)

		resolved, err := svc.ResolveChatConfig(context.Background(), chatID)
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if resolved.ChatModel != "chat-override-8b" || resolved.SystemOneModel != "julia-1" {
			t.Fatalf("expected chat model overrides to win: %+v", resolved)
		}
		if resolved.InteractionProfile != ProfileReactionsHigh || resolved.DecisionInstructions != "Never discuss politics." {
			t.Fatalf("expected chat profile/instructions overrides to win: %+v", resolved)
		}
	})

	t.Run("chat overrides survive cleared global defaults", func(t *testing.T) {
		// Clearing the global System One model must not disable the backend
		// where a per-chat override exists.
		store := &stubStore{
			global: GlobalSettings{},
			chat: ChatSettings{
				ChatID:             chatID,
				SystemOneModel:     "julia-1",
				InteractionProfile: ProfileTalkHigh,
			},
			chatFound: true,
		}
		svc := NewService(store, nil, nil)

		resolved, err := svc.ResolveChatConfig(context.Background(), chatID)
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if resolved.SystemOneModel != "julia-1" {
			t.Fatalf("per-chat decision model must survive a cleared global default, got %q", resolved.SystemOneModel)
		}
		if resolved.ChatModel != "" {
			t.Fatalf("expected unset generation model, got %q", resolved.ChatModel)
		}
	})

	t.Run("empty chat values inherit the global profile", func(t *testing.T) {
		store := &stubStore{
			global:    GlobalSettings{DefaultInteractionProfile: ProfileTalkHigh, DecisionInstructions: "global"},
			chat:      ChatSettings{ChatID: chatID},
			chatFound: true,
		}
		svc := NewService(store, nil, nil)

		resolved, err := svc.ResolveChatConfig(context.Background(), chatID)
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if resolved.InteractionProfile != ProfileTalkHigh || resolved.DecisionInstructions != "global" {
			t.Fatalf("empty chat fields must inherit: %+v", resolved)
		}
	})
}
