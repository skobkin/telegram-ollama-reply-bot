package sqlite

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"telegram-ollama-reply-bot/internal/adminconfig"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()

	store, err := Open(context.Background(), filepath.Join(t.TempDir(), "bot.sqlite"), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("open test store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	return store
}

func TestStoreGlobalFieldsAndPromptOverrides(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)

	global, err := store.GetGlobalSettings(ctx)
	if err != nil {
		t.Fatalf("get global settings: %v", err)
	}
	if global.DefaultInteractivityMode != adminconfig.InteractivityDisabled {
		t.Fatalf("expected disabled default interactivity, got %q", global.DefaultInteractivityMode)
	}

	if err := store.SetGlobalField(ctx, "character_name", "foxbot", 42); err != nil {
		t.Fatalf("set global field: %v", err)
	}
	global, err = store.GetGlobalSettings(ctx)
	if err != nil {
		t.Fatalf("get updated global settings: %v", err)
	}
	if global.CharacterName != "foxbot" {
		t.Fatalf("unexpected character_name: %q", global.CharacterName)
	}

	if err := store.SetPromptTemplate(ctx, adminconfig.PromptFeatureChat, 123, "chat {{.CharacterName}}", 42); err != nil {
		t.Fatalf("set prompt template: %v", err)
	}
	body, ok, err := store.GetPromptTemplate(ctx, adminconfig.PromptFeatureChat, 123)
	if err != nil {
		t.Fatalf("get prompt template: %v", err)
	}
	if !ok || body != "chat {{.CharacterName}}" {
		t.Fatalf("unexpected chat prompt override: ok=%v body=%q", ok, body)
	}
}

func TestStoreChatCatalogAndWhitelist(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)

	if err := store.UpsertChatCatalog(ctx, adminconfig.ChatCatalogEntry{
		ChatID:      100,
		ChatType:    "supergroup",
		Title:       "Test chat",
		DisplayName: "Test chat",
	}); err != nil {
		t.Fatalf("upsert chat catalog: %v", err)
	}

	chats, err := store.ListChats(ctx)
	if err != nil {
		t.Fatalf("list chats: %v", err)
	}
	if len(chats) != 1 || chats[0].ChatID != 100 {
		t.Fatalf("unexpected chats: %+v", chats)
	}

	if err := store.AddWhitelistChat(ctx, 100, 42); err != nil {
		t.Fatalf("add whitelist chat: %v", err)
	}
	ok, err := store.IsChatWhitelisted(ctx, 100)
	if err != nil {
		t.Fatalf("check whitelist: %v", err)
	}
	if !ok {
		t.Fatalf("expected chat to be whitelisted")
	}
}
