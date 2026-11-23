package bot

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"telegram-ollama-reply-bot/internal/adminconfig"
	"telegram-ollama-reply-bot/internal/config"
	"telegram-ollama-reply-bot/internal/reminders"
	"telegram-ollama-reply-bot/internal/state"
	"telegram-ollama-reply-bot/internal/statistics"
	"telegram-ollama-reply-bot/internal/support/markdown"

	tg "github.com/mymmrac/telego"
)

const bootTestToken = "1:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

// slowTelegram answers the Bot API with controllable sendMessage and getMe
// latencies, so a test can order the two round trips.
func slowTelegram(t *testing.T, sendDelay, getMeDelay time.Duration) *httptest.Server {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch method := strings.TrimPrefix(r.URL.Path, "/bot"+bootTestToken+"/"); method {
		case "getMe":
			time.Sleep(getMeDelay)
			_, _ = w.Write([]byte(`{"ok":true,"result":{"id":999,"is_bot":true,"first_name":"TestBot","username":"testbot"}}`))
		case "sendMessage":
			time.Sleep(sendDelay)
			_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":321,"date":1700000000,"chat":{"id":100500,"type":"private"},"text":"Reminder: buy milk"}}`))
		case "getUpdates":
			time.Sleep(100 * time.Millisecond)
			_, _ = w.Write([]byte(`{"ok":true,"result":[]}`))
		default:
			_, _ = w.Write([]byte(`{"ok":true,"result":true}`))
		}
	}))
	t.Cleanup(server.Close)

	return server
}

// overdueReminderStore holds one reminder that fell due while the bot was down.
type overdueReminderStore struct{ reminder reminders.Reminder }

func (s *overdueReminderStore) CreateReminder(context.Context, reminders.Reminder) error {
	return nil
}

func (s *overdueReminderStore) ListActiveReminders(context.Context, int64) ([]reminders.Reminder, error) {
	return nil, nil
}

func (s *overdueReminderStore) GetReminder(context.Context, string) (reminders.Reminder, bool, error) {
	return reminders.Reminder{}, false, nil
}

func (s *overdueReminderStore) LoadActiveReminders(context.Context) ([]reminders.Reminder, error) {
	return []reminders.Reminder{s.reminder}, nil
}

func (s *overdueReminderStore) UpdateReminderDelivery(context.Context, string, time.Time, time.Time, bool) error {
	return nil
}

func (s *overdueReminderStore) CancelReminder(context.Context, string) error { return nil }

// apiReminderSender is the reminder sender without the event publication.
type apiReminderSender struct{ api *tg.Bot }

func (s apiReminderSender) SendMessage(ctx context.Context, params *tg.SendMessageParams) (*tg.Message, error) {
	return s.api.SendMessage(ctx, params)
}

// A reminder that fell due during downtime is delivered by the scheduler right
// after a restart, while Bot.Run is still on its way to resolving the bot
// identity. App wiring seeds the identity before attaching the recorder, so the
// entry must carry the real bot name whichever round trip finishes first: here
// sendMessage returns long before Run's getMe does, the order that used to
// record an empty name (and race with Run's assignment under -race).
func TestReminderDeliveredDuringBotStartupCarriesTheBotIdentity(t *testing.T) {
	server := slowTelegram(t, 50*time.Millisecond, 400*time.Millisecond)
	api, err := tg.NewBot(bootTestToken, tg.WithAPIServer(server.URL), tg.WithDiscardLogger())
	if err != nil {
		t.Fatalf("create bot api: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	history := newMemoryConversationStore()
	bot := &Bot{
		ctx:       ctx,
		logger:    reactionTestLogger(),
		cfg:       config.BotConfig{AdminIDs: []int64{7}},
		history:   history,
		stats:     statistics.NewCollector(statistics.Config{}),
		admin:     adminconfig.NewService(&dmCatalogSpy{}, nil, nil),
		api:       api,
		sanitizer: markdown.NewTgMarkdownV2Sanitizer(),
	}

	scope := state.ConversationScope{ChatID: 100500}
	service := reminders.NewService(&overdueReminderStore{reminder: reminders.Reminder{
		ID:        "rem_1",
		Scope:     scope,
		Text:      "buy milk",
		Schedule:  reminders.Schedule{Type: reminders.ScheduleTypeOneShot},
		NextDueAt: time.Now().Add(-time.Hour).UTC(),
		Active:    true,
	}}, apiReminderSender{api: api}, nil, nil, reactionTestLogger())

	// The wiring order of internal/app: the scheduler is already running, the
	// identity (verified at startup) is seeded, the recorder is attached, and
	// only then does Run resolve the identity itself.
	go func() { _ = service.Run(ctx) }()
	bot.SetIdentity(&tg.User{ID: 999, IsBot: true, FirstName: "TestBot", Username: "testbot"})
	service.AttachDeliveryRecorder(bot)

	runDone := make(chan error, 1)
	go func() { runDone <- bot.Run() }()

	var recorded state.Message
	deadline := time.After(5 * time.Second)
	for {
		if messages := history.Snapshot(scope).Messages; len(messages) > 0 {
			recorded = messages[0]

			break
		}
		select {
		case <-deadline:
			t.Fatal("the delivered reminder was never recorded in history")
		case <-time.After(5 * time.Millisecond):
		}
	}

	if !recorded.IsMe || recorded.Name != "TestBot" || recorded.Username != "testbot" {
		t.Fatalf("a reminder recorded during startup must carry the bot identity: %+v", recorded)
	}
	if recorded.Text != "Reminder: buy milk" || recorded.MessageID != 321 {
		t.Fatalf("recorded entry = %+v", recorded)
	}

	// Let Run finish its own getMe (the write that raced) before teardown.
	time.Sleep(450 * time.Millisecond)
	cancel()
	select {
	case <-runDone:
	case <-time.After(10 * time.Second):
		t.Log("Run did not return after cancel (teardown only)")
	}
}
