package reminders

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"telegram-ollama-reply-bot/internal/state"

	t "github.com/mymmrac/telego"
)

type storeStub struct {
	items map[string]Reminder
}

func newStoreStub() *storeStub {
	return &storeStub{items: map[string]Reminder{}}
}

func (s *storeStub) CreateReminder(_ context.Context, reminder Reminder) error {
	s.items[reminder.ID] = reminder

	return nil
}

func (s *storeStub) ListActiveReminders(_ context.Context, chatID int64) ([]Reminder, error) {
	result := make([]Reminder, 0)
	for _, reminder := range s.items {
		if reminder.Active && reminder.Scope.ChatID == chatID {
			result = append(result, reminder)
		}
	}

	return result, nil
}

func (s *storeStub) GetReminder(_ context.Context, reminderID string) (Reminder, bool, error) {
	reminder, ok := s.items[reminderID]

	return reminder, ok, nil
}

func (s *storeStub) LoadActiveReminders(_ context.Context) ([]Reminder, error) {
	result := make([]Reminder, 0)
	for _, reminder := range s.items {
		if reminder.Active {
			result = append(result, reminder)
		}
	}

	return result, nil
}

func (s *storeStub) UpdateReminderDelivery(_ context.Context, reminderID string, deliveredAt time.Time, nextDueAt time.Time, active bool) error {
	reminder := s.items[reminderID]
	reminder.LastDeliveredAt = deliveredAt
	reminder.NextDueAt = nextDueAt
	reminder.Active = active
	reminder.UpdatedAt = deliveredAt
	s.items[reminderID] = reminder

	return nil
}

func (s *storeStub) CancelReminder(_ context.Context, reminderID string) error {
	reminder := s.items[reminderID]
	reminder.Active = false
	s.items[reminderID] = reminder

	return nil
}

type senderStub struct {
	sent []*string
}

func (s *senderStub) SendMessage(_ context.Context, params *t.SendMessageParams) (*t.Message, error) {
	text := params.Text
	s.sent = append(s.sent, &text)

	return &t.Message{}, nil
}

func TestAddReminderComputesMonthlySchedule(t *testing.T) {
	store := newStoreStub()
	svc := NewService(store, &senderStub{}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	svc.now = func() time.Time { return time.Date(2026, 4, 20, 10, 0, 0, 0, time.UTC) }

	reminder, err := svc.AddReminder(context.Background(), state.ConversationScope{ChatID: 1}, 7, CreateRequest{
		Text:         "pay rent",
		ScheduleType: "monthly",
		Timezone:     "Europe/Moscow",
		DayOfMonth:   20,
		TimeOfDay:    "19:00",
	})
	if err != nil {
		t.Fatalf("AddReminder() error = %v", err)
	}
	if !reminder.Active {
		t.Fatal("expected reminder to be active")
	}
	if reminder.NextDueAt.IsZero() {
		t.Fatal("expected next due time")
	}
}

func TestRemoveReminderChecksOwnership(t *testing.T) {
	store := newStoreStub()
	reminder := Reminder{
		ID:            "rem_1",
		Scope:         state.ConversationScope{ChatID: 1},
		CreatorUserID: 7,
		Text:          "x",
		Schedule:      Schedule{Type: ScheduleTypeOneShot},
		NextDueAt:     time.Now().Add(time.Hour),
		Active:        true,
	}
	store.items[reminder.ID] = reminder

	svc := NewService(store, &senderStub{}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if _, err := svc.RemoveReminder(context.Background(), 8, reminder.ID); err == nil {
		t.Fatal("expected ownership error")
	}
}

func TestComputeDeliveryWindowKeepsOnlyLatestMissedRecurringOccurrence(t *testing.T) {
	reminder := Reminder{
		ID:        "rem_1",
		Text:      "water plants",
		NextDueAt: time.Date(2026, 4, 20, 8, 0, 0, 0, time.UTC),
		Schedule: Schedule{
			Type:          ScheduleTypeIntervalDays,
			IntervalValue: 1,
		},
		Active: true,
	}

	occurrence, next, active, err := computeDeliveryWindow(reminder, time.Date(2026, 4, 23, 9, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("computeDeliveryWindow() error = %v", err)
	}
	if !active {
		t.Fatal("expected recurring reminder to stay active")
	}
	if occurrence.Day() != 23 {
		t.Fatalf("expected latest missed occurrence on day 23, got %s", occurrence)
	}
	if next.Day() != 24 {
		t.Fatalf("expected next occurrence on day 24, got %s", next)
	}
}
