package sqlite

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"telegram-ollama-reply-bot/internal/adminconfig"
	"telegram-ollama-reply-bot/internal/reminders"
	"telegram-ollama-reply-bot/internal/state"
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

func TestOpenConfiguresSingleSQLiteConnection(t *testing.T) {
	store := openTestStore(t)

	stats := store.db.Stats()
	if stats.MaxOpenConnections != 1 {
		t.Fatalf("expected single sqlite connection, got max open connections %d", stats.MaxOpenConnections)
	}

	var foreignKeys int
	if err := store.db.QueryRowContext(context.Background(), `PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil {
		t.Fatalf("read foreign_keys pragma: %v", err)
	}
	if foreignKeys != 1 {
		t.Fatalf("expected foreign_keys pragma enabled, got %d", foreignKeys)
	}

	var busyTimeout int
	if err := store.db.QueryRowContext(context.Background(), `PRAGMA busy_timeout`).Scan(&busyTimeout); err != nil {
		t.Fatalf("read busy_timeout pragma: %v", err)
	}
	if busyTimeout != 5000 {
		t.Fatalf("expected busy_timeout pragma 5000, got %d", busyTimeout)
	}
}

func TestStoreConcurrentChatCatalogUpserts(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)

	const workers = 32
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for i := range workers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs <- store.UpsertChatCatalog(ctx, adminconfig.ChatCatalogEntry{
				ChatID:      int64(100 + i%4),
				ChatType:    "supergroup",
				Title:       "Test chat",
				DisplayName: "Test chat",
			})
		}(i)
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent upsert chat catalog: %v", err)
		}
	}

	chats, err := store.ListChats(ctx)
	if err != nil {
		t.Fatalf("list chats: %v", err)
	}
	if len(chats) != 4 {
		t.Fatalf("expected 4 catalog entries, got %d: %+v", len(chats), chats)
	}
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

func TestStoreChatLanguageAndGenderOverrides(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)

	if err := store.SetChatField(ctx, 123, "character_name", "kitsune", 42); err != nil {
		t.Fatalf("set chat character_name: %v", err)
	}
	if err := store.SetChatField(ctx, 123, "language", "English", 42); err != nil {
		t.Fatalf("set chat language: %v", err)
	}
	if err := store.SetChatField(ctx, 123, "gender", "female", 42); err != nil {
		t.Fatalf("set chat gender: %v", err)
	}

	chat, ok, err := store.GetChatSettings(ctx, 123)
	if err != nil {
		t.Fatalf("get chat settings: %v", err)
	}
	if !ok {
		t.Fatalf("expected chat settings to exist")
	}
	if chat.CharacterName != "kitsune" || chat.Language != "English" || chat.Gender != "female" {
		t.Fatalf("unexpected chat persona overrides: %+v", chat)
	}

	if err := store.ClearChatField(ctx, 123, "character_name", 42); err != nil {
		t.Fatalf("clear chat character_name: %v", err)
	}
	if err := store.ClearChatField(ctx, 123, "language", 42); err != nil {
		t.Fatalf("clear chat language: %v", err)
	}
	if err := store.ClearChatField(ctx, 123, "gender", 42); err != nil {
		t.Fatalf("clear chat gender: %v", err)
	}

	chat, ok, err = store.GetChatSettings(ctx, 123)
	if err != nil {
		t.Fatalf("get cleared chat settings: %v", err)
	}
	if !ok {
		t.Fatalf("expected chat settings to remain")
	}
	if chat.CharacterName != "" || chat.Language != "" || chat.Gender != "" {
		t.Fatalf("expected cleared persona overrides, got %+v", chat)
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

func TestStoreReminderLifecycle(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)

	createdAt := time.Date(2026, 4, 20, 10, 0, 0, 0, time.UTC)
	reminder := reminders.Reminder{
		ID:            "rem_123",
		Scope:         state.ConversationScope{ChatID: 100, TopicID: 55},
		CreatorUserID: 42,
		Text:          "pay lessons",
		Schedule: reminders.Schedule{
			Type:             reminders.ScheduleTypeMonthly,
			Timezone:         "Europe/Moscow",
			DayOfMonth:       20,
			TimeOfDayMinutes: 19 * 60,
		},
		NextDueAt: createdAt.Add(9 * time.Hour),
		Active:    true,
		CreatedAt: createdAt,
		UpdatedAt: createdAt,
	}
	if err := store.CreateReminder(ctx, reminder); err != nil {
		t.Fatalf("create reminder: %v", err)
	}

	items, err := store.ListActiveReminders(ctx, 100)
	if err != nil {
		t.Fatalf("list reminders: %v", err)
	}
	if len(items) != 1 || items[0].ID != reminder.ID || items[0].Scope.TopicID != 55 {
		t.Fatalf("unexpected reminders: %+v", items)
	}

	fetched, ok, err := store.GetReminder(ctx, reminder.ID)
	if err != nil {
		t.Fatalf("get reminder: %v", err)
	}
	if !ok || fetched.Schedule.Type != reminders.ScheduleTypeMonthly {
		t.Fatalf("unexpected reminder fetch: ok=%v reminder=%+v", ok, fetched)
	}

	deliveredAt := createdAt.Add(10 * time.Hour)
	nextDue := createdAt.AddDate(0, 1, 0)
	if err := store.UpdateReminderDelivery(ctx, reminder.ID, deliveredAt, nextDue, true); err != nil {
		t.Fatalf("update reminder delivery: %v", err)
	}

	fetched, ok, err = store.GetReminder(ctx, reminder.ID)
	if err != nil {
		t.Fatalf("get updated reminder: %v", err)
	}
	if !ok || !fetched.LastDeliveredAt.Equal(deliveredAt) || !fetched.NextDueAt.Equal(nextDue) {
		t.Fatalf("unexpected updated reminder: %+v", fetched)
	}

	if err := store.CancelReminder(ctx, reminder.ID); err != nil {
		t.Fatalf("cancel reminder: %v", err)
	}
	items, err = store.ListActiveReminders(ctx, 100)
	if err != nil {
		t.Fatalf("list reminders after cancel: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("expected no active reminders, got %+v", items)
	}
}
