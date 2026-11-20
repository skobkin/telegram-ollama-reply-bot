package tooluse

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"telegram-ollama-reply-bot/internal/state"
	"telegram-ollama-reply-bot/internal/state/memory"
)

func contextRuntime(t *testing.T, scope state.ConversationScope, messages ...state.Message) *Runtime {
	t.Helper()
	store := memory.New(memory.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil))).Conversations()
	for _, msg := range messages {
		store.AppendMessage(scope, msg)
	}

	return &Runtime{history: store}
}

func callContext(t *testing.T, r *Runtime, scope state.ConversationScope, payload string) (toolResult, error) {
	t.Helper()

	return r.getMessageContext(context.Background(), CallContext{Scope: scope}, json.RawMessage(payload))
}

func viewIDs(views []map[string]any) []int {
	ids := make([]int, 0, len(views))
	for _, view := range views {
		ids = append(ids, view["message_id"].(int))
	}

	return ids
}

func seqMessages(ids ...int) []state.Message {
	base := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	messages := make([]state.Message, 0, len(ids))
	for i, id := range ids {
		messages = append(messages, state.Message{Name: "u", FromID: int64(id), Text: "m", MessageID: id, CreatedAt: base.Add(time.Duration(i) * time.Minute)})
	}

	return messages
}

func TestMessageContextNearestFirstAndWindowFlags(t *testing.T) {
	scope := state.ConversationScope{ChatID: 1, TopicID: 7}
	r := contextRuntime(t, scope, seqMessages(10, 11, 12, 13, 14, 15)...)

	result, err := callContext(t, r, scope, `{"message_id":13,"before":2,"after":5}`)
	if err != nil || result.Status != "ok" {
		t.Fatalf("%+v %v", result.Error, err)
	}
	data := result.Data.(map[string]any)
	if got := viewIDs(data["before"].([]map[string]any)); !slices.Equal(got, []int{12, 11}) {
		t.Errorf("before = %v", got)
	}
	if got := viewIDs(data["after"].([]map[string]any)); !slices.Equal(got, []int{14, 15}) {
		t.Errorf("after = %v", got)
	}
	window := data["retained_window"].(map[string]any)
	if window["start_reached"] != false || window["end_reached"] != true || window["retained_messages"] != 6 {
		t.Errorf("window = %v", window)
	}
	if data["topic_id"] != 7 || data["target"].(map[string]any)["message_id"] != 13 {
		t.Errorf("data = %v", data)
	}

	result, _ = callContext(t, r, scope, `{"message_id":10,"before":3,"after":0}`)
	window = result.Data.(map[string]any)["retained_window"].(map[string]any)
	if window["start_reached"] != true || window["end_reached"] != false {
		t.Errorf("window = %v", window)
	}
}

func TestMessageContextOrderIgnoresInsertionOrderAndMergesDuplicates(t *testing.T) {
	scope := state.ConversationScope{ChatID: 1}
	messages := seqMessages(30, 10, 20, 40)
	// A duplicate of 20 carries the longer text and reply link; 0-ID entries are skipped.
	messages = append(messages,
		state.Message{Name: "u", Text: "much longer text", MessageID: 20, ReplyTo: &state.Message{MessageID: 10}},
		state.Message{Name: "noid", Text: "x"},
	)
	r := contextRuntime(t, scope, messages...)

	result, _ := callContext(t, r, scope, `{"message_id":20,"before":5,"after":5}`)
	data := result.Data.(map[string]any)
	if got := viewIDs(data["before"].([]map[string]any)); !slices.Equal(got, []int{10}) {
		t.Errorf("before = %v", got)
	}
	if got := viewIDs(data["after"].([]map[string]any)); !slices.Equal(got, []int{30, 40}) {
		t.Errorf("after = %v", got)
	}
	target := data["target"].(map[string]any)
	if target["text_preview"] != "much longer text" || target["reply_to_message_id"] != 10 {
		t.Errorf("duplicate not merged: %v", target)
	}
}

func TestMessageContextMissingAndForeignTopic(t *testing.T) {
	scope := state.ConversationScope{ChatID: 1, TopicID: 1}
	other := state.ConversationScope{ChatID: 1, TopicID: 2}
	r := contextRuntime(t, scope, seqMessages(10)...)
	r.history.AppendMessage(other, state.Message{Name: "x", Text: "secret", MessageID: 99})

	for _, id := range []string{"11", "99"} {
		result, err := callContext(t, r, scope, `{"message_id":`+id+`}`)
		if err != nil || result.Error == nil || result.Error.Code != "message_not_in_history" || result.Error.Class != ErrorNotFound || result.Error.Retryable {
			t.Errorf("id %s: %+v %v", id, result.Error, err)
		}
		if text := marshalResult(result, messageContextResultCharBudget); strings.Contains(text, "secret") {
			t.Error("foreign topic content leaked")
		}
	}
}

func TestMessageContextArgumentValidation(t *testing.T) {
	scope := state.ConversationScope{ChatID: 1}
	r := contextRuntime(t, scope, seqMessages(10)...)
	for _, payload := range []string{`{}`, `{"message_id":0}`, `{"message_id":-1}`, `{"message_id":1,"before":-1}`, `{"message_id":1,"after":11}`, `{"message_id":"1"}`, `{"message_id":1,"x":1}`} {
		if _, err := callContext(t, r, scope, payload); err == nil {
			t.Errorf("%s: expected invalid arguments", payload)
		}
	}
}

func TestMessageTextTruncatedReflectsRealShortening(t *testing.T) {
	natural := state.Message{Text: "ends with an ellipsis…"}
	if preview, shortened := threadMessagePreview(natural); shortened || preview != natural.Text {
		t.Errorf("natural ellipsis flagged: %q %v", preview, shortened)
	}
	exact := state.Message{Text: strings.Repeat("a", messageThreadSnippetCharLimit)}
	if _, shortened := threadMessagePreview(exact); shortened {
		t.Error("text exactly at the limit must not be flagged")
	}
	long := state.Message{Text: strings.Repeat("a", messageThreadSnippetCharLimit+1)}
	if _, shortened := threadMessagePreview(long); !shortened {
		t.Error("shortened text must be flagged")
	}
	if view := threadMessageView(natural); view["text_truncated"] != false {
		t.Errorf("thread view flagged natural ellipsis: %v", view)
	}
}

func TestMessageContextAuthorFieldsAreClampedWithFlags(t *testing.T) {
	view := messageContextView(state.Message{MessageID: 1, Name: strings.Repeat("n", 65), Username: strings.Repeat("u", 32)})
	if view["name_truncated"] != true || view["username_truncated"] != false || len([]rune(view["name"].(string))) != messageContextNameRunes {
		t.Errorf("view = %v", view)
	}
}

func hostileMessage(id int, replyTo int) state.Message {
	msg := state.Message{
		Name:      strings.Repeat("<", 200),
		Username:  strings.Repeat("\"", 100),
		Text:      strings.Repeat("\\<\"", 300),
		MessageID: id,
		FromID:    1 << 52,
		CreatedAt: time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC),
	}
	if replyTo != 0 {
		msg.ReplyTo = &state.Message{MessageID: replyTo}
	}

	return msg
}

func TestMessageContextWorstCaseTargetStaysDeliverable(t *testing.T) {
	scope := state.ConversationScope{ChatID: 1}
	messages := make([]state.Message, 0, 21)
	for id := 100; id <= 120; id++ {
		messages = append(messages, hostileMessage(id, id-1))
	}
	r := contextRuntime(t, scope, messages...)

	result, err := callContext(t, r, scope, `{"message_id":110,"before":10,"after":10}`)
	if err != nil || result.Status != "ok" {
		t.Fatalf("%+v %v", result.Error, err)
	}
	text, preparation := prepareResult(result, messageContextResultCharBudget)
	if preparation == ResultPreparationFailed {
		t.Fatalf("worst case failed delivery: %.120s", text)
	}
	if n := len([]rune(text)); n > messageContextResultCharBudget {
		t.Fatalf("over budget: %d", n)
	}
	var decoded struct {
		Truncated bool `json:"truncated"`
		Data      struct {
			Target struct {
				MessageID        int   `json:"message_id"`
				ReplyToMessageID int   `json:"reply_to_message_id"`
				FromID           int64 `json:"from_id"`
				NameTruncated    bool  `json:"name_truncated"`
				TextTruncated    bool  `json:"text_truncated"`
			} `json:"target"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(text), &decoded); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	target := decoded.Data.Target
	if target.MessageID != 110 || target.ReplyToMessageID != 109 || target.FromID != 1<<52 || !target.NameTruncated || !target.TextTruncated {
		t.Errorf("target damaged: %+v truncated=%v", target, decoded.Truncated)
	}
}

func TestMessageContextTargetAloneAlwaysFits(t *testing.T) {
	scope := state.ConversationScope{ChatID: 1}
	r := contextRuntime(t, scope, hostileMessage(5, 4))
	result, _ := callContext(t, r, scope, `{"message_id":5}`)
	if _, preparation := prepareResult(result, messageContextResultCharBudget); preparation != ResultPreparationComplete {
		t.Fatalf("target-only worst case = %s", preparation)
	}
}

// The window flags describe the neighbours actually serialized: when the
// budget forces neighbours out, a boundary that was reached by the initial
// selection must not still be reported as reached.
func TestMessageContextWindowFlagsMatchBudgetFittedNeighbours(t *testing.T) {
	scope := state.ConversationScope{ChatID: 1}
	messages := make([]state.Message, 0, 21)
	for id := 100; id <= 120; id++ {
		messages = append(messages, hostileMessage(id, id-1))
	}
	r := contextRuntime(t, scope, messages...)

	// Without budget pressure the initial selection reaches both boundaries.
	small := contextRuntime(t, scope, seqMessages(100, 101, 102)...)
	smallResult, _ := callContext(t, small, scope, `{"message_id":101,"before":3,"after":3}`)
	window := smallResult.Data.(map[string]any)["retained_window"].(map[string]any)
	if window["start_reached"] != true || window["end_reached"] != true {
		t.Fatalf("unpressured window = %v", window)
	}

	result, err := callContext(t, r, scope, `{"message_id":110,"before":10,"after":10}`)
	if err != nil || result.Status != "ok" {
		t.Fatalf("%+v %v", result.Error, err)
	}
	data := result.Data.(map[string]any)
	before := data["before"].([]map[string]any)
	after := data["after"].([]map[string]any)
	if len(before) == 10 && len(after) == 10 {
		t.Fatal("expected the budget to remove neighbours")
	}
	window = data["retained_window"].(map[string]any)
	if window["start_reached"] != (110-len(before) == 100) || window["end_reached"] != (110+len(after) == 120) {
		t.Errorf("flags %v inconsistent with %d before / %d after", window, len(before), len(after))
	}
	if window["start_reached"] != false || window["end_reached"] != false {
		t.Errorf("a removed boundary message is still reported as reached: %v", window)
	}
	if _, preparation := prepareResult(result, messageContextResultCharBudget); preparation != ResultPreparationComplete {
		t.Errorf("fitted result should need no further reduction, got %s", preparation)
	}
	if got := data["requested"].(map[string]any); got["before"] != 10 || got["after"] != 10 {
		t.Errorf("requested = %v", got)
	}
}

// Neighbours removed to fit the budget are reported explicitly in the
// serialized result, and the target stays intact.
func TestMessageContextBudgetOmissionsAreReported(t *testing.T) {
	scope := state.ConversationScope{ChatID: 1}
	messages := make([]state.Message, 0, 21)
	for id := 100; id <= 120; id++ {
		messages = append(messages, hostileMessage(id, id-1))
	}
	r := contextRuntime(t, scope, messages...)

	result, err := callContext(t, r, scope, `{"message_id":110,"before":10,"after":10}`)
	if err != nil || result.Status != "ok" {
		t.Fatalf("%+v %v", result.Error, err)
	}
	text, preparation := prepareResult(result, messageContextResultCharBudget)
	if preparation == ResultPreparationFailed {
		t.Fatalf("delivery failed: %.120s", text)
	}
	var decoded struct {
		Truncated  bool           `json:"truncated"`
		Reductions map[string]int `json:"reductions"`
		Data       struct {
			Target struct {
				MessageID        int `json:"message_id"`
				ReplyToMessageID int `json:"reply_to_message_id"`
			} `json:"target"`
			Before []json.RawMessage `json:"before"`
			After  []json.RawMessage `json:"after"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(text), &decoded); err != nil {
		t.Fatal(err)
	}
	if !decoded.Truncated {
		t.Fatal("omissions are not flagged as truncated")
	}
	if got, want := decoded.Reductions["data.before"], 10-len(decoded.Data.Before); got != want {
		t.Errorf("before omitted = %d, want %d", got, want)
	}
	if got, want := decoded.Reductions["data.after"], 10-len(decoded.Data.After); got != want {
		t.Errorf("after omitted = %d, want %d", got, want)
	}
	if decoded.Reductions["data.before"]+decoded.Reductions["data.after"] == 0 {
		t.Error("no omissions reported")
	}
	if decoded.Data.Target.MessageID != 110 || decoded.Data.Target.ReplyToMessageID != 109 {
		t.Errorf("target damaged: %+v", decoded.Data.Target)
	}
	if preparation != ResultPreparationComplete {
		t.Errorf("metadata-aware fitting should need no further reduction, got %s", preparation)
	}

	// No budget pressure: nothing is reported.
	small := contextRuntime(t, scope, seqMessages(1, 2, 3)...)
	ok, _ := callContext(t, small, scope, `{"message_id":2}`)
	if ok.Truncated || ok.Reductions != nil {
		t.Errorf("unpressured result flagged: %+v", ok)
	}
}
