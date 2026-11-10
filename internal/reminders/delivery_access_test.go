package reminders

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"telegram-ollama-reply-bot/internal/state"
)

func accessTestReminder(id string, chatID int64, schedule Schedule, dueAt time.Time) Reminder {
	return Reminder{
		ID:        id,
		Scope:     state.ConversationScope{ChatID: chatID},
		Text:      "secret text",
		Schedule:  schedule,
		NextDueAt: dueAt,
		Active:    true,
	}
}

func dueAccessTestService(t *testing.T, canDeliver CanDeliverFunc) (*Service, *storeStub, *senderStub) {
	t.Helper()

	store := newStoreStub()
	sender := &senderStub{}
	svc := NewService(store, sender, []int64{7}, canDeliver, slog.New(slog.NewTextHandler(io.Discard, nil)))
	svc.now = func() time.Time { return time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC) }

	return svc, store, sender
}

// TestDeliverReminderAllowedChatDelivers proves the gate passes ordinary
// allowed chats through unchanged.
func TestDeliverReminderAllowedChatDelivers(t *testing.T) {
	due := time.Date(2026, 11, 10, 11, 0, 0, 0, time.UTC)
	svc, _, sender := dueAccessTestService(t, func(context.Context, int64) (bool, error) {
		return true, nil
	})

	reminder := accessTestReminder("rem_ok", -100123, Schedule{Type: ScheduleTypeIntervalDays, IntervalValue: 1}, due)
	if err := svc.deliverReminder(context.Background(), reminder); err != nil {
		t.Fatalf("deliverReminder() error = %v", err)
	}
	if len(sender.sent) != 1 {
		t.Fatalf("expected one delivery, got %d", len(sender.sent))
	}
}

// TestDeliverReminderAdminDMPassesGate proves a CanDeliver closure that
// exempts admin DMs keeps admin reminders flowing (the wiring in app.go does
// this; the service contract is that true means deliver).
func TestDeliverReminderAdminDMPassesGate(t *testing.T) {
	due := time.Date(2026, 11, 10, 11, 0, 0, 0, time.UTC)
	svc, _, sender := dueAccessTestService(t, func(_ context.Context, chatID int64) (bool, error) {
		return chatID == 7, nil // admin DM only, mirroring the app wiring
	})

	reminder := accessTestReminder("rem_admin", 7, Schedule{Type: ScheduleTypeOneShot}, due)
	if err := svc.deliverReminder(context.Background(), reminder); err != nil {
		t.Fatalf("deliverReminder() error = %v", err)
	}
	if len(sender.sent) != 1 {
		t.Fatalf("expected admin DM delivery, got %d sends", len(sender.sent))
	}
}

// TestDeliverReminderDenialSkipsAndAdvances proves a policy denial sends
// nothing, advances a recurring schedule, and completes a one-shot — with no
// message content anywhere.
func TestDeliverReminderDenialSkipsAndAdvances(t *testing.T) {
	due := time.Date(2026, 11, 10, 11, 0, 0, 0, time.UTC)
	svc, store, sender := dueAccessTestService(t, func(context.Context, int64) (bool, error) {
		return false, nil
	})

	recurring := accessTestReminder("rem_daily", -100123, Schedule{Type: ScheduleTypeIntervalDays, IntervalValue: 1}, due)
	store.items[recurring.ID] = recurring
	if err := svc.deliverReminder(context.Background(), recurring); err != nil {
		t.Fatalf("deliverReminder() error = %v", err)
	}

	oneShot := accessTestReminder("rem_once", 55, Schedule{Type: ScheduleTypeOneShot}, due)
	store.items[oneShot.ID] = oneShot
	if err := svc.deliverReminder(context.Background(), oneShot); err != nil {
		t.Fatalf("deliverReminder() error = %v", err)
	}

	if len(sender.sent) != 0 {
		t.Fatalf("denied reminders must not be sent, got %d sends", len(sender.sent))
	}

	storedRecurring := store.items[recurring.ID]
	if !storedRecurring.Active {
		t.Fatalf("recurring reminder must stay active after denial")
	}
	if !storedRecurring.NextDueAt.Equal(time.Date(2026, 11, 11, 11, 0, 0, 0, time.UTC)) {
		t.Fatalf("recurring schedule must advance one day, got %s", storedRecurring.NextDueAt)
	}

	storedOneShot := store.items[oneShot.ID]
	if storedOneShot.Active {
		t.Fatalf("one-shot reminder must complete after denial")
	}
}

// TestDeliverReminderAccessErrorRetriesWithBackoff proves a transient lookup
// error fails closed without marking the occurrence delivered, and that the
// scheduler sleeps through the backoff instead of spinning.
func TestDeliverReminderAccessErrorRetriesWithBackoff(t *testing.T) {
	due := time.Date(2026, 11, 10, 11, 0, 0, 0, time.UTC)
	svc, store, sender := dueAccessTestService(t, func(context.Context, int64) (bool, error) {
		return false, errors.New("db unavailable")
	})

	reminder := accessTestReminder("rem_flaky", -100123, Schedule{Type: ScheduleTypeOneShot}, due)
	store.items[reminder.ID] = reminder
	svc.mu.Lock()
	svc.reminders[reminder.ID] = reminder
	svc.mu.Unlock()

	err := svc.deliverReminder(context.Background(), reminder)
	if err == nil {
		t.Fatalf("expected access-check error to surface")
	}

	if len(sender.sent) != 0 {
		t.Fatalf("nothing must be sent on access error, got %d", len(sender.sent))
	}
	stored := store.items[reminder.ID]
	if !stored.Active || !stored.NextDueAt.Equal(due) {
		t.Fatalf("occurrence must stay pending, got active=%v next=%s", stored.Active, stored.NextDueAt)
	}

	// The reminder is backed off: not due for immediate processing...
	svc.processDue(context.Background())
	if len(sender.sent) != 0 {
		t.Fatalf("backed-off reminder must not be processed, got %d sends", len(sender.sent))
	}

	// ...and the scheduler waits until the backoff expires rather than
	// returning an immediate zero wait.
	if wait := svc.nextWait(); wait <= 0 || wait > accessRetryBackoff {
		t.Fatalf("expected backoff-bounded wait, got %s", wait)
	}

	// After the backoff elapses the reminder is processed again (still
	// failing closed on a persistent error — never delivered, never spun).
	svc.mu.Lock()
	svc.nextAttempt[reminder.ID] = svc.now().Add(-time.Second)
	svc.mu.Unlock()
	svc.processDue(context.Background())
	if len(sender.sent) != 0 {
		t.Fatalf("persistent access error must keep failing closed, got %d sends", len(sender.sent))
	}
	stored = store.items[reminder.ID]
	if !stored.Active {
		t.Fatalf("persistent access error must not complete the reminder")
	}
}

// TestNilCanDeliverDeliversUnchanged proves backward compatibility when no
// gate is wired.
func TestNilCanDeliverDeliversUnchanged(t *testing.T) {
	due := time.Date(2026, 11, 10, 11, 0, 0, 0, time.UTC)
	svc, _, sender := dueAccessTestService(t, nil)

	reminder := accessTestReminder("rem_nogate", -100123, Schedule{Type: ScheduleTypeOneShot}, due)
	if err := svc.deliverReminder(context.Background(), reminder); err != nil {
		t.Fatalf("deliverReminder() error = %v", err)
	}
	if len(sender.sent) != 1 {
		t.Fatalf("expected delivery without gate, got %d", len(sender.sent))
	}
}
