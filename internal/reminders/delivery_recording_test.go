package reminders

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"telegram-ollama-reply-bot/internal/state"

	tg "github.com/mymmrac/telego"
)

type recorderStub struct {
	mu    sync.Mutex
	calls []recordedMessage
}

type recordedMessage struct {
	scope state.ConversationScope
	msg   state.Message
}

func (r *recorderStub) RecordBotMessage(_ context.Context, scope state.ConversationScope, msg state.Message) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, recordedMessage{scope: scope, msg: msg})
}

func (r *recorderStub) recorded() []recordedMessage {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]recordedMessage(nil), r.calls...)
}

// configurableSender returns a caller-chosen message or error.
type configurableSender struct {
	message *tg.Message
	err     error
}

func (s *configurableSender) SendMessage(context.Context, *tg.SendMessageParams) (*tg.Message, error) {
	return s.message, s.err
}

func dueReminder() Reminder {
	return Reminder{
		ID:        "rem_1",
		Scope:     state.ConversationScope{ChatID: -100500, TopicID: 9},
		Text:      "water plants",
		Schedule:  Schedule{Type: ScheduleTypeOneShot},
		NextDueAt: time.Date(2026, 4, 20, 9, 0, 0, 0, time.UTC),
		Active:    true,
	}
}

func recordingService(sender Sender, canDeliver CanDeliverFunc) (*Service, *storeStub, *recorderStub) {
	store := newStoreStub()
	svc := NewService(store, sender, nil, canDeliver, slog.New(slog.NewTextHandler(io.Discard, nil)))
	svc.now = func() time.Time { return time.Date(2026, 4, 20, 10, 0, 0, 0, time.UTC) }
	recorder := &recorderStub{}
	svc.AttachDeliveryRecorder(recorder)

	return svc, store, recorder
}

func TestDeliveredReminderIsRecordedInHistory(t *testing.T) {
	t.Parallel()

	sentAt := time.Date(2026, 4, 20, 10, 0, 1, 0, time.UTC)
	svc, store, recorder := recordingService(&configurableSender{message: &tg.Message{MessageID: 321, Date: sentAt.Unix()}}, nil)

	if err := svc.deliverReminder(context.Background(), dueReminder()); err != nil {
		t.Fatal(err)
	}

	calls := recorder.recorded()
	if len(calls) != 1 {
		t.Fatalf("expected one recorded message, got %+v", calls)
	}
	got := calls[0]
	if got.scope != (state.ConversationScope{ChatID: -100500, TopicID: 9}) {
		t.Errorf("scope = %+v", got.scope)
	}
	if got.msg.Text != "Reminder: water plants" || got.msg.MessageID != 321 || !got.msg.CreatedAt.Equal(sentAt) {
		t.Errorf("message = %+v", got.msg)
	}
	if store.items["rem_1"].Active {
		t.Error("a delivered one-shot reminder must complete")
	}
}

func TestReminderThatWasNotSentIsNotRecorded(t *testing.T) {
	t.Parallel()

	t.Run("send error", func(t *testing.T) {
		t.Parallel()

		svc, store, recorder := recordingService(&configurableSender{err: errors.New("telegram down")}, nil)
		if err := svc.deliverReminder(context.Background(), dueReminder()); err == nil {
			t.Fatal("expected the send error")
		}
		if got := recorder.recorded(); len(got) != 0 {
			t.Fatalf("a failed send must not be recorded: %+v", got)
		}
		if _, touched := store.items["rem_1"]; touched {
			t.Fatal("a failed send must not advance the schedule")
		}
	})

	t.Run("unconfirmed message", func(t *testing.T) {
		t.Parallel()

		svc, _, recorder := recordingService(&configurableSender{message: &tg.Message{}}, nil)
		if err := svc.deliverReminder(context.Background(), dueReminder()); err != nil {
			t.Fatal(err)
		}
		if got := recorder.recorded(); len(got) != 0 {
			t.Fatalf("a message without an id confirms nothing: %+v", got)
		}
	})

	t.Run("no message returned", func(t *testing.T) {
		t.Parallel()

		svc, _, recorder := recordingService(&configurableSender{}, nil)
		if err := svc.deliverReminder(context.Background(), dueReminder()); err != nil {
			t.Fatal(err)
		}
		if got := recorder.recorded(); len(got) != 0 {
			t.Fatalf("recorded without a message: %+v", got)
		}
	})

	t.Run("denied by access policy", func(t *testing.T) {
		t.Parallel()

		svc, _, recorder := recordingService(&configurableSender{message: &tg.Message{MessageID: 5}},
			func(context.Context, int64) (bool, error) { return false, nil })
		if err := svc.deliverReminder(context.Background(), dueReminder()); err != nil {
			t.Fatal(err)
		}
		if got := recorder.recorded(); len(got) != 0 {
			t.Fatalf("nothing is sent for a denied chat, so nothing is recorded: %+v", got)
		}
	})
}

func TestDeliveryWithoutRecorderStillWorks(t *testing.T) {
	t.Parallel()

	store := newStoreStub()
	svc := NewService(store, &configurableSender{message: &tg.Message{MessageID: 7}}, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	svc.now = func() time.Time { return time.Date(2026, 4, 20, 10, 0, 0, 0, time.UTC) }

	if err := svc.deliverReminder(context.Background(), dueReminder()); err != nil {
		t.Fatal(err)
	}
	if store.items["rem_1"].Active {
		t.Fatal("delivery must complete without a recorder")
	}
}

// The recorder is attached while the scheduler may already be delivering; the
// race detector must stay quiet.
func TestAttachingTheRecorderDuringDeliveryIsSafe(t *testing.T) {
	t.Parallel()

	svc := NewService(newStoreStub(), &configurableSender{message: &tg.Message{MessageID: 7}}, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	svc.now = func() time.Time { return time.Date(2026, 4, 20, 10, 0, 0, 0, time.UTC) }

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			svc.AttachDeliveryRecorder(&recorderStub{})
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			_ = svc.deliverReminder(context.Background(), dueReminder())
		}
	}()
	wg.Wait()
}
