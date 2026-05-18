package bot

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"telegram-ollama-reply-bot/internal/adminconfig"
	"telegram-ollama-reply-bot/internal/config"

	tg "github.com/mymmrac/telego"
)

type adminStoreStub struct {
	adminconfig.GlobalSettings
	chatSettings adminconfig.ChatSettings
	chatFound    bool
}

func (s *adminStoreStub) GetGlobalSettings(context.Context) (adminconfig.GlobalSettings, error) {
	return s.GlobalSettings, nil
}
func (s *adminStoreStub) SetGlobalField(context.Context, string, string, int64) error { return nil }
func (s *adminStoreStub) GetChatSettings(context.Context, int64) (adminconfig.ChatSettings, bool, error) {
	return s.chatSettings, s.chatFound, nil
}
func (s *adminStoreStub) SetChatField(context.Context, int64, string, string, int64) error {
	return nil
}
func (s *adminStoreStub) ClearChatField(context.Context, int64, string, int64) error { return nil }
func (s *adminStoreStub) GetPromptTemplate(context.Context, adminconfig.PromptFeature, int64) (string, bool, error) {
	return "", false, nil
}
func (s *adminStoreStub) SetPromptTemplate(context.Context, adminconfig.PromptFeature, int64, string, int64) error {
	return nil
}
func (s *adminStoreStub) ClearPromptTemplate(context.Context, adminconfig.PromptFeature, int64) error {
	return nil
}
func (s *adminStoreStub) UpsertChatCatalog(context.Context, adminconfig.ChatCatalogEntry) error {
	return nil
}
func (s *adminStoreStub) ListChats(context.Context) ([]adminconfig.ChatCatalogEntry, error) {
	return nil, nil
}
func (s *adminStoreStub) AddWhitelistChat(context.Context, int64, int64) error   { return nil }
func (s *adminStoreStub) RemoveWhitelistChat(context.Context, int64) error       { return nil }
func (s *adminStoreStub) ListWhitelistChats(context.Context) ([]int64, error)    { return nil, nil }
func (s *adminStoreStub) IsChatWhitelisted(context.Context, int64) (bool, error) { return false, nil }

func TestShouldProcessChatMessageDisabledByDefault(t *testing.T) {
	b := &Bot{
		cfg: config.BotConfig{AdminIDs: []int64{1}},
		admin: adminconfig.NewService(&adminStoreStub{
			GlobalSettings: adminconfig.GlobalSettings{
				CharacterName:            "bot",
				Language:                 "Russian",
				Gender:                   "neutral",
				ToneMode:                 "default",
				DefaultInteractivityMode: adminconfig.InteractivityDisabled,
			},
		}),
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	message := tg.Message{
		Text: "hello bot",
		Chat: tg.Chat{ID: 100, Type: tg.ChatTypeSupergroup},
		From: &tg.User{ID: 2},
	}

	if b.shouldProcessChatMessage(context.Background(), message) {
		t.Fatalf("expected disabled interactivity to block processing")
	}
}

func TestShouldProcessChatMessageUsesCharacterNameTrigger(t *testing.T) {
	b := &Bot{
		cfg: config.BotConfig{AdminIDs: []int64{1}},
		admin: adminconfig.NewService(&adminStoreStub{
			GlobalSettings: adminconfig.GlobalSettings{
				CharacterName:            "bot",
				Language:                 "Russian",
				Gender:                   "neutral",
				ToneMode:                 "default",
				DefaultInteractivityMode: adminconfig.InteractivityMentionsOnly,
			},
			chatFound: true,
			chatSettings: adminconfig.ChatSettings{
				CharacterName: "kitsune",
			},
		}),
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	message := tg.Message{
		Text: "hey kitsune",
		Chat: tg.Chat{ID: 100, Type: tg.ChatTypeSupergroup},
		From: &tg.User{ID: 2},
	}

	if !b.shouldProcessChatMessage(context.Background(), message) {
		t.Fatalf("expected character name soft trigger to allow processing")
	}
}

func TestAdminControlsDisabledTextIncludesUserID(t *testing.T) {
	t.Parallel()

	text := adminControlsDisabledText(&tg.User{ID: 123456789})
	expected := "Admin controls are disabled: BOT_ADMIN_IDS is empty.\nYour Telegram user ID: 123456789\nSet BOT_ADMIN_IDS=123456789"

	if text != expected {
		t.Fatalf("unexpected text: expected %q, got %q", expected, text)
	}
}

func TestAdminControlsDisabledTextWithoutUser(t *testing.T) {
	t.Parallel()

	text := adminControlsDisabledText(nil)
	expected := "Admin controls are disabled: BOT_ADMIN_IDS is empty."

	if text != expected {
		t.Fatalf("unexpected text: expected %q, got %q", expected, text)
	}
}

func TestFormatConfigFieldsIncludesInteractivityModes(t *testing.T) {
	t.Parallel()

	text := formatConfigFields([]string{"language", "interactivity_mode"})
	expected := "language\ninteractivity_mode (disabled, mentions_only, mentions_or_replies)"

	if text != expected {
		t.Fatalf("unexpected text: expected %q, got %q", expected, text)
	}
}
