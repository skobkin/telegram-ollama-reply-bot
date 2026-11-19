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
		if resolved.ChatModel != "" || resolved.SystemOneModel != "" || resolved.DecisionInstructions != "" ||
			resolved.SummarizeModel != "" || resolved.ImageRecognitionModel != "" {
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
		if resolved.SummarizeModel != "summarizer-8b" || resolved.ImageRecognitionModel != "vision-mini" {
			t.Fatalf("global-only summarize and image models must resolve for every chat: %+v", resolved)
		}
		if resolved.InteractionProfile != ProfileTalkLow || resolved.DecisionInstructions != "Be curious." {
			t.Fatalf("unexpected profile/instructions: %+v", resolved)
		}
	})

	t.Run("summarize and image recognition have no chat overrides", func(t *testing.T) {
		// The global defaults are the effective values for those features;
		// a chat row cannot carry them at all.
		store := &stubStore{
			global: GlobalSettings{
				SummarizeModel:        "summarizer-8b",
				ImageRecognitionModel: "vision-mini",
			},
			chat:      ChatSettings{ChatID: chatID, ChatModel: "chat-override-8b"},
			chatFound: true,
		}
		svc := NewService(store, nil, nil)

		resolved, err := svc.ResolveChatConfig(context.Background(), chatID)
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if resolved.SummarizeModel != "summarizer-8b" || resolved.ImageRecognitionModel != "vision-mini" {
			t.Fatalf("global-only models must stay in effect: %+v", resolved)
		}
		if resolved.ChatModel != "chat-override-8b" {
			t.Fatalf("chat generation override must still win: %+v", resolved)
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

// perChatStore varies chat settings per chat ID for the count and view tests.
type perChatStore struct {
	stubStore
	settings map[int64]ChatSettings
	found    map[int64]bool
}

func (s *perChatStore) GetChatSettings(_ context.Context, chatID int64) (ChatSettings, bool, error) {
	if found, ok := s.found[chatID]; ok {
		return s.settings[chatID], found, nil
	}

	return ChatSettings{ChatID: chatID}, false, nil
}

// TestEffectiveChatConfigModelFields pins the chat-card view of the new
// fields: overrides flag as chat source, unset fields show the global value
// they would inherit.
func TestEffectiveChatConfigModelFields(t *testing.T) {
	chatID := int64(555)
	global := GlobalSettings{
		ChatModel:                 "gemma4:e4b",
		SystemOneModel:            "laya",
		DefaultInteractionProfile: ProfileTalkLow,
		DecisionInstructions:      "Be curious.",
	}

	t.Run("overrides win and flag", func(t *testing.T) {
		store := &perChatStore{
			stubStore: stubStore{global: global},
			settings: map[int64]ChatSettings{chatID: {
				ChatID:             chatID,
				ChatModel:          "chat-override-8b",
				SystemOneModel:     "julia-1",
				InteractionProfile: ProfileReactionsHigh,
			}},
			found: map[int64]bool{chatID: true},
		}
		svc := NewService(store, nil, nil)

		view, err := svc.EffectiveChatConfig(context.Background(), chatID)
		if err != nil {
			t.Fatalf("view: %v", err)
		}

		want := map[string]string{
			"chat_model":          "chat-override-8b",
			"systemone_model":     "julia-1",
			"interaction_profile": string(ProfileReactionsHigh),
		}
		for name, expected := range want {
			field := viewField(view, name)
			if field.Effective != expected || !field.Overridden {
				t.Fatalf("%s must show the override %q flagged, got %q flagged=%v", name, expected, field.Effective, field.Overridden)
			}
		}
		// decision_instructions has no override here: it shows the global value.
		field := viewField(view, "decision_instructions")
		if field.Effective != "Be curious." || field.Overridden {
			t.Fatalf("unset chat instructions must inherit flagged=false, got %q flagged=%v", field.Effective, field.Overridden)
		}
	})

	t.Run("no chat row inherits every global value", func(t *testing.T) {
		store := &perChatStore{stubStore: stubStore{global: global}}
		svc := NewService(store, nil, nil)

		view, err := svc.EffectiveChatConfig(context.Background(), chatID)
		if err != nil {
			t.Fatalf("view: %v", err)
		}

		want := map[string]string{
			"chat_model":            "gemma4:e4b",
			"systemone_model":       "laya",
			"interaction_profile":   string(ProfileTalkLow),
			"decision_instructions": "Be curious.",
		}
		for name, expected := range want {
			field := viewField(view, name)
			if field.Effective != expected || field.Overridden {
				t.Fatalf("%s must inherit %q unflagged, got %q flagged=%v", name, expected, field.Effective, field.Overridden)
			}
		}
	})
}

func viewField(view EffectiveChatConfig, name string) EffectiveField {
	for _, field := range view.Fields {
		if field.Spec.Name == name {
			return field
		}
	}

	panic("view is missing field " + name)
}

// TestCountChatsInheritingProfile pins the scope-confirmation count: chats
// without a row and chats with an empty profile inherit the global default;
// chats with their own profile are excluded.
func TestCountChatsInheritingProfile(t *testing.T) {
	const (
		noRow    = int64(101)
		emptyRow = int64(102)
		overridn = int64(103)
	)

	store := &perChatStore{
		stubStore: stubStore{chats: []ChatCatalogEntry{
			{ChatID: noRow, ChatType: "supergroup", Title: "No row"},
			{ChatID: emptyRow, ChatType: "supergroup", Title: "Empty profile"},
			{ChatID: overridn, ChatType: "supergroup", Title: "Overridden"},
		}},
		settings: map[int64]ChatSettings{
			emptyRow: {ChatID: emptyRow},
			overridn: {ChatID: overridn, InteractionProfile: ProfileTalkHigh},
		},
		found: map[int64]bool{emptyRow: true, overridn: true},
	}
	svc := NewService(store, nil, nil)

	count, err := svc.CountChatsInheritingProfile(context.Background())
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 2 {
		t.Fatalf("no-row and empty-profile chats must count as inheriting, got %d", count)
	}
}
