package tooluse

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"telegram-ollama-reply-bot/internal/llm"
	"telegram-ollama-reply-bot/internal/state"
	"telegram-ollama-reply-bot/internal/state/memory"

	tg "github.com/mymmrac/telego"
)

const sentDate = int64(1700000000)

var recordingScope = state.ConversationScope{ChatID: -100500, TopicID: 7}

func newRecordingRuntime(t *testing.T, sender *stubActionSender) (*Runtime, state.ConversationStore) {
	t.Helper()

	history := memory.New(memory.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil))).Conversations()
	runtime := New(&stubLLM{}, history, &stubExtractor{}, nil, sender, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), Config{
		MaxIterations: 6,
		BotIdentity:   BotIdentity{UserID: 999, Name: "Kitsu Bot", Username: "kitsubot"},
	})

	return runtime, history
}

func recordingCall() CallContext {
	return CallContext{Scope: recordingScope, run: &runState{}}
}

// onlyRecorded returns the single history entry or fails the test.
func onlyRecorded(t *testing.T, history state.ConversationStore) state.Message {
	t.Helper()

	messages := history.Snapshot(recordingScope).Messages
	if len(messages) != 1 {
		t.Fatalf("expected exactly one recorded message, got %d: %+v", len(messages), messages)
	}

	return messages[0]
}

// requireBotEntry checks the fields every recorded outgoing message shares.
func requireBotEntry(t *testing.T, msg state.Message, messageID int) {
	t.Helper()

	if !msg.IsMe || msg.Name != "Kitsu Bot" || msg.Username != "kitsubot" {
		t.Errorf("bot authorship missing: %+v", msg)
	}
	if msg.MessageID != messageID {
		t.Errorf("message id = %d, want Telegram's %d", msg.MessageID, messageID)
	}
	if want := time.Unix(sentDate, 0).UTC(); !msg.CreatedAt.Equal(want) {
		t.Errorf("created at = %v, want Telegram's timestamp %v", msg.CreatedAt, want)
	}
	if msg.ChatID != recordingScope.ChatID || msg.TopicID != recordingScope.TopicID {
		t.Errorf("scope = %d/%d, want %d/%d", msg.ChatID, msg.TopicID, recordingScope.ChatID, recordingScope.TopicID)
	}
	if msg.Text != "" || msg.ReplyTo != nil || msg.HasImage || msg.ImageMeta != nil || msg.IsUserRequest {
		t.Errorf("the attachment must be the only content, with no reply link or image data: %+v", msg)
	}
}

func TestSendPollRecordsTheConfirmedMessage(t *testing.T) {
	t.Parallel()

	sender := &stubActionSender{message: &tg.Message{MessageID: 55, Date: sentDate}}
	runtime, history := newRecordingRuntime(t, sender)

	if _, err := runtime.sendPoll(context.Background(), recordingCall(), json.RawMessage(`{"question":"Dinner?","options":["Pizza","Sushi"]}`)); err != nil {
		t.Fatal(err)
	}

	msg := onlyRecorded(t, history)
	requireBotEntry(t, msg, 55)
	if msg.Attachment == nil || msg.Attachment.Kind != state.AttachmentPoll || msg.Attachment.Label != `"Dinner?" — Pizza / Sushi` {
		t.Fatalf("attachment = %+v", msg.Attachment)
	}

	// Scope isolation: the entry lives in the topic it was sent to only.
	if got := history.Snapshot(state.ConversationScope{ChatID: recordingScope.ChatID}).Messages; len(got) != 0 {
		t.Fatalf("recorded into the wrong topic: %+v", got)
	}
}

func TestSendQuizRecordsWithoutRevealingTheAnswer(t *testing.T) {
	t.Parallel()

	sender := &stubActionSender{message: &tg.Message{MessageID: 56, Date: sentDate}}
	runtime, history := newRecordingRuntime(t, sender)

	args := `{"question":"2+2?","options":["3","4"],"correct_option_index":1,"explanation":"Because arithmetic."}`
	if _, err := runtime.sendQuiz(context.Background(), recordingCall(), json.RawMessage(args)); err != nil {
		t.Fatal(err)
	}

	msg := onlyRecorded(t, history)
	requireBotEntry(t, msg, 56)
	if msg.Attachment == nil || msg.Attachment.Kind != state.AttachmentQuiz || msg.Attachment.Label != `"2+2?" — 3 / 4` {
		t.Fatalf("attachment = %+v", msg.Attachment)
	}
}

func TestSendDiceRecordsTheRolledValue(t *testing.T) {
	t.Parallel()

	sender := &stubActionSender{message: &tg.Message{MessageID: 57, Date: sentDate, Dice: &tg.Dice{Emoji: "🎯", Value: 6}}}
	runtime, history := newRecordingRuntime(t, sender)

	if _, err := runtime.sendDice(context.Background(), recordingCall(), json.RawMessage(`{"emoji":"🎯"}`)); err != nil {
		t.Fatal(err)
	}

	msg := onlyRecorded(t, history)
	requireBotEntry(t, msg, 57)
	if msg.Attachment == nil || msg.Attachment.Kind != state.AttachmentDice || msg.Attachment.Label != "🎯 = 6" {
		t.Fatalf("attachment = %+v", msg.Attachment)
	}
}

func TestSendDiceWithoutReportedOutcomeDoesNotClaimAValue(t *testing.T) {
	t.Parallel()

	sender := &stubActionSender{message: &tg.Message{MessageID: 58, Date: sentDate}}
	runtime, history := newRecordingRuntime(t, sender)

	if _, err := runtime.sendDice(context.Background(), recordingCall(), json.RawMessage(`{"emoji":"🎲"}`)); err != nil {
		t.Fatal(err)
	}
	if msg := onlyRecorded(t, history); msg.Attachment == nil || msg.Attachment.Label != "🎲" {
		t.Fatalf("attachment = %+v", msg.Attachment)
	}
}

// The chart is described from its spec: no image metadata is retained, so the
// vision model is never asked to describe the bot's own chart.
func TestGenerateChartRecordsADescriptionWithoutImageData(t *testing.T) {
	t.Parallel()

	sender := &stubActionSender{message: &tg.Message{MessageID: 59, Date: sentDate, Photo: []tg.PhotoSize{{FileID: "secret-file", FileUniqueID: "u"}}}}
	runtime, history := newRecordingRuntime(t, sender)

	if _, err := runtime.generateChart(context.Background(), recordingCall(), json.RawMessage(chartArgsJSON)); err != nil {
		t.Fatal(err)
	}

	msg := onlyRecorded(t, history)
	requireBotEntry(t, msg, 59)
	if msg.Attachment == nil || msg.Attachment.Kind != state.AttachmentChart || msg.Attachment.Label != "bar: Votes" {
		t.Fatalf("attachment = %+v", msg.Attachment)
	}
}

func TestSendStickerRecordsTheCatalogDescription(t *testing.T) {
	sender := &stubActionSender{message: &tg.Message{MessageID: 77, Date: sentDate}}
	runtime, _ := directSticker(t, sender)
	history := memory.New(memory.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil))).Conversations()
	runtime.history = history
	runtime.config.BotIdentity = BotIdentity{UserID: 999, Name: "Kitsu Bot", Username: "kitsubot"}

	call := CallContext{Scope: recordingScope, run: &runState{}}
	if result := sendStickerID(t, runtime, call, "sticker_01"); result.Status != "ok" {
		t.Fatalf("unexpected failure: %+v", result.Error)
	}

	msg := onlyRecorded(t, history)
	requireBotEntry(t, msg, 77)
	if msg.Attachment == nil || msg.Attachment.Kind != state.AttachmentSticker || msg.Attachment.Label != "🦊 (Happy fox waving hello)" {
		t.Fatalf("attachment = %+v", msg.Attachment)
	}
	// The Telegram file id used for sending must never reach history.
	if msg.ImageMeta != nil || msg.HasImage {
		t.Fatalf("sticker entries must carry no file data: %+v", msg)
	}
}

// Failed or unconfirmed sends never create history entries.
func TestUnconfirmedSendsAreNotRecorded(t *testing.T) {
	t.Parallel()

	pollArgs := json.RawMessage(`{"question":"Dinner?","options":["Pizza","Sushi"]}`)

	t.Run("send error", func(t *testing.T) {
		t.Parallel()

		runtime, history := newRecordingRuntime(t, &stubActionSender{err: errors.New("telegram down")})
		if _, err := runtime.sendPoll(context.Background(), recordingCall(), pollArgs); err == nil {
			t.Fatal("expected the send error")
		}
		if got := history.Snapshot(recordingScope).Messages; len(got) != 0 {
			t.Fatalf("failed send must not be recorded: %+v", got)
		}
	})

	t.Run("message without id", func(t *testing.T) {
		t.Parallel()

		runtime, history := newRecordingRuntime(t, &stubActionSender{message: &tg.Message{MessageID: 0}})
		if _, err := runtime.sendPoll(context.Background(), recordingCall(), pollArgs); err != nil {
			t.Fatal(err)
		}
		if got := history.Snapshot(recordingScope).Messages; len(got) != 0 {
			t.Fatalf("an unconfirmed send must not be recorded: %+v", got)
		}
	})

	t.Run("chart without a returned message", func(t *testing.T) {
		t.Parallel()

		runtime, history := newRecordingRuntime(t, &stubActionSender{})
		if _, err := runtime.generateChart(context.Background(), recordingCall(), json.RawMessage(chartArgsJSON)); err != nil {
			t.Fatal(err)
		}
		if got := history.Snapshot(recordingScope).Messages; len(got) != 0 {
			t.Fatalf("an unconfirmed chart must not be recorded: %+v", got)
		}
	})

	t.Run("rejected arguments", func(t *testing.T) {
		t.Parallel()

		runtime, history := newRecordingRuntime(t, &stubActionSender{message: &tg.Message{MessageID: 1}})
		if _, err := runtime.sendPoll(context.Background(), recordingCall(), json.RawMessage(`{"question":"x","options":["only one"]}`)); err == nil {
			t.Fatal("expected invalid arguments")
		}
		if got := history.Snapshot(recordingScope).Messages; len(got) != 0 {
			t.Fatalf("a rejected call must not be recorded: %+v", got)
		}
	})
}

type fakeRecorder struct {
	mu    sync.Mutex
	calls []recordedCall
}

type recordedCall struct {
	scope state.ConversationScope
	msg   state.Message
}

func (f *fakeRecorder) RecordBotMessage(_ context.Context, scope state.ConversationScope, msg state.Message) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, recordedCall{scope: scope, msg: msg})
}

// An attached recorder owns the append (the bot also replays buffered
// reactions there), so the runtime must not write to history a second time.
func TestAttachedRecorderReceivesTheMessageInsteadOfTheHistoryFallback(t *testing.T) {
	t.Parallel()

	sender := &stubActionSender{message: &tg.Message{MessageID: 60, Date: sentDate}}
	runtime, history := newRecordingRuntime(t, sender)
	recorder := &fakeRecorder{}
	runtime.AttachOutgoingRecorder(recorder)

	if _, err := runtime.sendPoll(context.Background(), recordingCall(), json.RawMessage(`{"question":"Dinner?","options":["Pizza","Sushi"]}`)); err != nil {
		t.Fatal(err)
	}

	if len(recorder.calls) != 1 {
		t.Fatalf("recorder calls = %d, want 1", len(recorder.calls))
	}
	got := recorder.calls[0]
	if got.scope != recordingScope || got.msg.MessageID != 60 || !got.msg.IsMe || got.msg.Attachment == nil {
		t.Fatalf("recorded call = %+v", got)
	}
	if stored := history.Snapshot(recordingScope).Messages; len(stored) != 0 {
		t.Fatalf("the runtime must not double-write when a recorder is attached: %+v", stored)
	}
}

// A whole tool-enabled run records the send once: execution events and the
// tool-call records do not feed history.
func TestReplyWithToolsRecordsASendExactlyOnce(t *testing.T) {
	t.Parallel()

	stub := &stubLLM{responses: []llm.Response{
		{Message: llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{
			{ID: "call-1", Name: "send_poll", Arguments: json.RawMessage(`{"question":"Dinner?","options":["Pizza","Sushi"]}`)},
		}}},
		{Message: llm.TextMessage(llm.RoleAssistant, "Vote above!")},
	}}
	sender := &stubActionSender{message: &tg.Message{MessageID: 61, Date: sentDate}}
	history := memory.New(memory.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil))).Conversations()
	runtime := New(stub, history, &stubExtractor{}, nil, sender, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), Config{
		MaxIterations: 6,
		BotIdentity:   BotIdentity{UserID: 999, Name: "Kitsu Bot", Username: "kitsubot"},
	})

	req := eventRequest()
	result, err := runtime.ReplyWithTools(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.ToolCalls) != 1 || !result.ToolCalls[0].Executed {
		t.Fatalf("tool records = %+v", result.ToolCalls)
	}

	messages := history.Snapshot(req.Scope).Messages
	if len(messages) != 1 || messages[0].MessageID != 61 || messages[0].Attachment == nil {
		t.Fatalf("expected exactly one recorded poll, got %+v", messages)
	}
}
