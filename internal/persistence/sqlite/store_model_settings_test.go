package sqlite

import (
	"context"
	"testing"

	"telegram-ollama-reply-bot/internal/adminconfig"
)

func TestStoreModelAndInteractivitySettingsRoundTrip(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	// Fresh schema defaults preserve pre-migration behavior.
	global, err := store.GetGlobalSettings(ctx)
	if err != nil {
		t.Fatalf("get global settings: %v", err)
	}
	if global.ChatModel != "" || global.SummarizeModel != "" || global.ImageRecognitionModel != "" || global.SystemOneModel != "" || global.DecisionInstructions != "" {
		t.Fatalf("expected unset model defaults, got %+v", global)
	}
	if global.DefaultInteractionProfile != adminconfig.ProfileNonInteractive {
		t.Fatalf("expected non_interactive default, got %q", global.DefaultInteractionProfile)
	}

	if err := store.SetGlobalField(ctx, "chat_model", "gemma4:e4b", 42); err != nil {
		t.Fatalf("set chat model: %v", err)
	}
	if err := store.SetGlobalField(ctx, "summarize_model", "summarizer-8b", 42); err != nil {
		t.Fatalf("set summarize model: %v", err)
	}
	if err := store.SetGlobalField(ctx, "image_recognition_model", "vision-mini", 42); err != nil {
		t.Fatalf("set image recognition model: %v", err)
	}
	if err := store.SetGlobalField(ctx, "systemone_model", "laya", 42); err != nil {
		t.Fatalf("set systemone model: %v", err)
	}
	if err := store.SetGlobalField(ctx, "default_interaction_profile", "talk_low", 42); err != nil {
		t.Fatalf("set interaction profile: %v", err)
	}
	if err := store.SetGlobalField(ctx, "decision_instructions", "Engage with programming topics.\nStay polite.", 42); err != nil {
		t.Fatalf("set decision instructions: %v", err)
	}

	global, err = store.GetGlobalSettings(ctx)
	if err != nil {
		t.Fatalf("re-read global settings: %v", err)
	}
	if global.ChatModel != "gemma4:e4b" || global.SummarizeModel != "summarizer-8b" ||
		global.ImageRecognitionModel != "vision-mini" || global.SystemOneModel != "laya" {
		t.Fatalf("unexpected model round-trip: %+v", global)
	}
	if global.DefaultInteractionProfile != adminconfig.ProfileTalkLow {
		t.Fatalf("unexpected profile: %q", global.DefaultInteractionProfile)
	}
	if global.DecisionInstructions != "Engage with programming topics.\nStay polite." {
		t.Fatalf("multi-line instructions must survive verbatim, got %q", global.DecisionInstructions)
	}

	// Clearing restores the unset state per field.
	for _, field := range []string{"chat_model", "summarize_model", "image_recognition_model", "systemone_model", "decision_instructions"} {
		if err := store.SetGlobalField(ctx, field, "", 42); err != nil {
			t.Fatalf("clear %s: %v", field, err)
		}
	}
	global, err = store.GetGlobalSettings(ctx)
	if err != nil {
		t.Fatalf("re-read cleared global settings: %v", err)
	}
	if global.ChatModel != "" || global.SystemOneModel != "" || global.DecisionInstructions != "" {
		t.Fatalf("expected cleared global fields, got %+v", global)
	}
	if global.DefaultInteractionProfile != adminconfig.ProfileTalkLow {
		t.Fatalf("profile has no unset state and must survive, got %q", global.DefaultInteractionProfile)
	}
}

func TestStoreRejectsInvalidInteractionProfile(t *testing.T) {
	store := openTestStore(t)

	if err := store.SetGlobalField(context.Background(), "default_interaction_profile", "chaotic", 42); err == nil {
		t.Fatal("expected invalid global profile to be rejected")
	}
	if err := store.SetChatField(context.Background(), 555, "interaction_profile", "chaotic", 42); err == nil {
		t.Fatal("expected invalid chat profile to be rejected")
	}
}

func TestStoreChatModelSettingsRoundTrip(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	const chatID = 777

	if _, found, err := store.GetChatSettings(ctx, chatID); err != nil || found {
		t.Fatalf("expected no chat row yet, found=%v err=%v", found, err)
	}

	if err := store.SetChatField(ctx, chatID, "chat_model", "chat-override-8b", 42); err != nil {
		t.Fatalf("set chat model: %v", err)
	}
	if err := store.SetChatField(ctx, chatID, "systemone_model", "julia-1", 42); err != nil {
		t.Fatalf("set systemone model: %v", err)
	}
	if err := store.SetChatField(ctx, chatID, "interaction_profile", "reactions_high", 42); err != nil {
		t.Fatalf("set interaction profile: %v", err)
	}
	if err := store.SetChatField(ctx, chatID, "decision_instructions", "Never discuss politics.", 42); err != nil {
		t.Fatalf("set decision instructions: %v", err)
	}

	chat, found, err := store.GetChatSettings(ctx, chatID)
	if err != nil || !found {
		t.Fatalf("expected chat row, found=%v err=%v", found, err)
	}
	if chat.ChatModel != "chat-override-8b" || chat.SystemOneModel != "julia-1" ||
		chat.InteractionProfile != adminconfig.ProfileReactionsHigh ||
		chat.DecisionInstructions != "Never discuss politics." {
		t.Fatalf("unexpected chat settings round-trip: %+v", chat)
	}

	// Clearing restores inherit ('') semantics per field.
	if err := store.ClearChatField(ctx, chatID, "systemone_model", 42); err != nil {
		t.Fatalf("clear systemone model: %v", err)
	}
	if err := store.ClearChatField(ctx, chatID, "interaction_profile", 42); err != nil {
		t.Fatalf("clear interaction profile: %v", err)
	}
	chat, _, err = store.GetChatSettings(ctx, chatID)
	if err != nil {
		t.Fatalf("re-read chat settings: %v", err)
	}
	if chat.SystemOneModel != "" || chat.InteractionProfile != "" {
		t.Fatalf("expected cleared chat fields, got %+v", chat)
	}
	if chat.ChatModel != "chat-override-8b" || chat.DecisionInstructions != "Never discuss politics." {
		t.Fatalf("cleared fields must not disturb siblings, got %+v", chat)
	}

	// Clearing everything still leaves the model fields back at inherit.
	if err := store.ClearChatField(ctx, chatID, "all", 42); err != nil {
		t.Fatalf("clear all: %v", err)
	}
	if _, found, err := store.GetChatSettings(ctx, chatID); err != nil || found {
		t.Fatalf("expected chat row deleted, found=%v err=%v", found, err)
	}
}
