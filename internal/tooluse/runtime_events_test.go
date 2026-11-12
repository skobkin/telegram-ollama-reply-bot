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

	"telegram-ollama-reply-bot/internal/execution"
	"telegram-ollama-reply-bot/internal/llm"
	"telegram-ollama-reply-bot/internal/state"
	"telegram-ollama-reply-bot/internal/state/memory"

	tg "github.com/mymmrac/telego"
)

// recordingDispatcher captures published events for assertions.
type recordingDispatcher struct {
	mu     sync.Mutex
	events []execution.Event
}

func (d *recordingDispatcher) Publish(event execution.Event) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.events = append(d.events, event)
}

func (d *recordingDispatcher) snapshot() []execution.Event {
	d.mu.Lock()
	defer d.mu.Unlock()

	return append([]execution.Event(nil), d.events...)
}

func eventsOf[T execution.Event](events []execution.Event) []T {
	var out []T
	for _, event := range events {
		if typed, ok := event.(T); ok {
			out = append(out, typed)
		}
	}

	return out
}

func newEventRuntime(t *testing.T, stub *stubLLM, dispatcher execution.Dispatcher) *Runtime {
	t.Helper()

	return New(
		stub,
		memory.New(memory.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil))).Conversations(),
		&stubExtractor{},
		nil,
		&stubActionSender{},
		nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		Config{MaxIterations: 6, Dispatcher: dispatcher},
	)
}

func eventRequest() ChatRequest {
	return ChatRequest{
		Scope:          state.ConversationScope{ChatID: 7, TopicID: 3},
		PromptScope:    llm.PromptScope{ChatID: 7},
		ReplyContext:   llm.ChatReplyContext{UserMessage: llm.TextMessage(llm.RoleUser, "hi")},
		RequestMessage: state.Message{FromID: 11, MessageID: 42},
		RunID:          "run-abc",
	}
}

func TestReplyWithToolsEmitsRunLifecycleForTextReply(t *testing.T) {
	recorder := &recordingDispatcher{}
	stub := &stubLLM{responses: []llm.Response{
		{Message: llm.TextMessage(llm.RoleAssistant, "Hello there"), Usage: llm.TokenUsage{TotalTokens: 5}},
	}}
	runtime := newEventRuntime(t, stub, recorder)

	result, err := runtime.ReplyWithTools(context.Background(), eventRequest())
	if err != nil {
		t.Fatalf("ReplyWithTools() error = %v", err)
	}
	if result.Reply != "Hello there" || result.Usage == nil {
		t.Fatalf("unexpected result: %+v", result)
	}

	events := recorder.snapshot()
	started := eventsOf[execution.RunStarted](events)
	completed := eventsOf[execution.RunCompleted](events)
	failed := eventsOf[execution.RunFailed](events)
	if len(started) != 1 || len(completed) != 1 || len(failed) != 0 {
		t.Fatalf("lifecycle events: started=%d completed=%d failed=%d", len(started), len(completed), len(failed))
	}

	rc := execution.RunContext{RunID: "run-abc", ChatID: 7, TopicID: 3, MessageID: 42}
	if started[0].RunContext != rc || completed[0].RunContext != rc {
		t.Fatalf("run context mismatch: %+v / %+v", started[0].RunContext, completed[0].RunContext)
	}
	if completed[0].ReplyLength != len([]rune("Hello there")) {
		t.Fatalf("reply length = %d", completed[0].ReplyLength)
	}

	generations := eventsOf[execution.GenerationStarted](events)
	generationDones := eventsOf[execution.GenerationCompleted](events)
	if len(generations) != 1 || len(generationDones) != 1 {
		t.Fatalf("generation events: started=%d completed=%d", len(generations), len(generationDones))
	}
	if generationDones[0].FinishReason != "" || generationDones[0].ToolCallCount != 0 {
		t.Fatalf("unexpected generation completion: %+v", generationDones[0])
	}

	// Ordering: RunStarted first, RunCompleted last.
	if _, ok := events[0].(execution.RunStarted); !ok {
		t.Fatalf("first event is %T, want RunStarted", events[0])
	}
	if _, ok := events[len(events)-1].(execution.RunCompleted); !ok {
		t.Fatalf("last event is %T, want RunCompleted", events[len(events)-1])
	}
}

func TestReplyWithToolsEmitsToolEventsAndRecords(t *testing.T) {
	recorder := &recordingDispatcher{}
	stub := &stubLLM{responses: []llm.Response{
		{
			Message: llm.Message{
				Role: llm.RoleAssistant,
				ToolCalls: []llm.ToolCall{
					{ID: "call-1", Name: "get_current_time", Arguments: json.RawMessage(`{}`)},
				},
			},
			FinishReason: "tool_calls",
		},
		{Message: llm.TextMessage(llm.RoleAssistant, "It is noon")},
	}}
	runtime := newEventRuntime(t, stub, recorder)

	result, err := runtime.ReplyWithTools(context.Background(), eventRequest())
	if err != nil {
		t.Fatalf("ReplyWithTools() error = %v", err)
	}
	if result.Reply != "It is noon" {
		t.Fatalf("unexpected reply: %q", result.Reply)
	}

	events := recorder.snapshot()
	starts := eventsOf[execution.ToolStarted](events)
	dones := eventsOf[execution.ToolCompleted](events)
	if len(starts) != 1 || len(dones) != 1 {
		t.Fatalf("tool events: started=%d completed=%d", len(starts), len(dones))
	}
	if starts[0].ToolName != "get_current_time" || starts[0].Internal {
		t.Fatalf("unexpected tool start: %+v", starts[0])
	}
	if dones[0].Status != execution.StatusOK || dones[0].Internal {
		t.Fatalf("unexpected tool completion: %+v", dones[0])
	}
	if dones[0].Duration <= 0 || dones[0].Duration > 5*time.Second {
		t.Fatalf("implausible duration: %v", dones[0].Duration)
	}

	// Ordering: start before completion, completion before run completion.
	startIdx, doneIdx := -1, -1
	for i, event := range events {
		switch event.(type) {
		case execution.ToolStarted:
			startIdx = i
		case execution.ToolCompleted:
			doneIdx = i
		}
	}
	if startIdx == -1 || doneIdx == -1 || startIdx > doneIdx || doneIdx > len(events)-2 {
		t.Fatalf("tool events out of order: start=%d done=%d total=%d", startIdx, doneIdx, len(events))
	}

	if len(result.ToolCalls) != 1 {
		t.Fatalf("tool call records: %d, want 1", len(result.ToolCalls))
	}
	record := result.ToolCalls[0]
	if record.CallID != "call-1" || record.Name != "get_current_time" ||
		record.Visibility != VisibilityStandard || record.Status != execution.StatusOK ||
		record.Duration != dones[0].Duration {
		t.Fatalf("unexpected record: %+v", record)
	}
}

func TestReplyWithToolsReportsStructuredFailureAsError(t *testing.T) {
	recorder := &recordingDispatcher{}
	stub := &stubLLM{responses: []llm.Response{
		{
			Message: llm.Message{
				Role: llm.RoleAssistant,
				ToolCalls: []llm.ToolCall{
					{ID: "call-1", Name: "get_current_time", Arguments: json.RawMessage(`{}`)},
				},
			},
		},
		{Message: llm.TextMessage(llm.RoleAssistant, "done")},
	}}
	runtime := newEventRuntime(t, stub, recorder)
	// Replace the built-in with a handler that reports a structured failure
	// with a nil Go error: success requires the result to indicate success.
	runtime.registry.Register(Definition{
		Name:             "get_current_time",
		Description:      "structured failure stub",
		Parameters:       json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
		ResultCharBudget: 600,
		Visibility:       VisibilityInternal,
		Handler: func(_ context.Context, _ CallContext, _ json.RawMessage) (toolResult, error) {
			return toolResult{Status: "error", Error: "structured failure"}, nil
		},
	})

	result, err := runtime.ReplyWithTools(context.Background(), eventRequest())
	if err != nil {
		t.Fatalf("ReplyWithTools() error = %v", err)
	}

	events := recorder.snapshot()
	dones := eventsOf[execution.ToolCompleted](events)
	if len(dones) != 1 || dones[0].Status != execution.StatusError || !dones[0].Internal {
		t.Fatalf("structured failure not surfaced: %+v", dones)
	}
	if len(result.ToolCalls) != 1 || result.ToolCalls[0].Status != execution.StatusError ||
		result.ToolCalls[0].Visibility != VisibilityInternal {
		t.Fatalf("unexpected record: %+v", result.ToolCalls)
	}
}

func TestReplyWithToolsEmitsEventsForUnknownTool(t *testing.T) {
	recorder := &recordingDispatcher{}
	stub := &stubLLM{responses: []llm.Response{
		{
			Message: llm.Message{
				Role: llm.RoleAssistant,
				ToolCalls: []llm.ToolCall{
					{ID: "call-1", Name: "no_such_tool", Arguments: json.RawMessage(`{}`)},
				},
			},
		},
		{Message: llm.TextMessage(llm.RoleAssistant, "recovered")},
	}}
	runtime := newEventRuntime(t, stub, recorder)

	result, err := runtime.ReplyWithTools(context.Background(), eventRequest())
	if err != nil {
		t.Fatalf("ReplyWithTools() error = %v", err)
	}
	if result.Reply != "recovered" {
		t.Fatalf("unexpected reply: %q", result.Reply)
	}

	dones := eventsOf[execution.ToolCompleted](recorder.snapshot())
	if len(dones) != 1 || dones[0].Status != execution.StatusError || dones[0].ToolName != "no_such_tool" {
		t.Fatalf("unknown tool event: %+v", dones)
	}
	if len(result.ToolCalls) != 1 || result.ToolCalls[0].Status != execution.StatusError {
		t.Fatalf("unknown tool record: %+v", result.ToolCalls)
	}
}

func TestReplyWithToolsCompletesSideEffectOnlyRunWithoutReply(t *testing.T) {
	recorder := &recordingDispatcher{}
	stub := &stubLLM{responses: []llm.Response{
		{
			Message: llm.Message{
				Role: llm.RoleAssistant,
				ToolCalls: []llm.ToolCall{
					{ID: "call-1", Name: "send_dice", Arguments: json.RawMessage(`{}`)},
				},
			},
		},
		{Message: llm.Message{Role: llm.RoleAssistant}},
	}}
	runtime := New(
		stub,
		memory.New(memory.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil))).Conversations(),
		&stubExtractor{},
		nil,
		&stubActionSender{diceMessage: &tg.Message{MessageID: 55, Dice: &tg.Dice{Emoji: "🎲", Value: 6}}},
		nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		Config{MaxIterations: 6, Dispatcher: recorder},
	)

	result, err := runtime.ReplyWithTools(context.Background(), eventRequest())
	if err != nil {
		t.Fatalf("ReplyWithTools() error = %v", err)
	}
	if result.Reply != "" {
		t.Fatalf("unexpected reply: %q", result.Reply)
	}

	completed := eventsOf[execution.RunCompleted](recorder.snapshot())
	if len(completed) != 1 || completed[0].ReplyLength != 0 {
		t.Fatalf("side-effect-only completion: %+v", completed)
	}
}

func TestReplyWithToolsClassifiesFailureReasons(t *testing.T) {
	request := eventRequest()
	baseResponse := llm.Response{Message: llm.TextMessage(llm.RoleAssistant, "ok")}

	tests := []struct {
		name       string
		stub       *stubLLM
		wantReason string
	}{
		{
			name:       "prepare failure",
			stub:       &stubLLM{buildErr: errors.New("no prompt")},
			wantReason: execution.ReasonPrepare,
		},
		{
			name:       "backend failure",
			stub:       &stubLLM{generateErr: errors.New("backend down")},
			wantReason: execution.ReasonBackend,
		},
		{
			name:       "timeout",
			stub:       &stubLLM{generateErr: context.DeadlineExceeded},
			wantReason: execution.ReasonTimeout,
		},
		{
			name:       "cancellation",
			stub:       &stubLLM{generateErr: context.Canceled},
			wantReason: execution.ReasonCancelled,
		},
		{
			name:       "no choices",
			stub:       &stubLLM{responses: []llm.Response{{Message: llm.Message{Role: llm.RoleAssistant}}}},
			wantReason: execution.ReasonNoChoices,
		},
		{
			name: "loop limit",
			stub: &stubLLM{responses: []llm.Response{
				{
					Message: llm.Message{
						Role:      llm.RoleAssistant,
						ToolCalls: []llm.ToolCall{{ID: "call-1", Name: "get_current_time", Arguments: json.RawMessage(`{}`)}},
					},
				},
			}},
			wantReason: execution.ReasonLoopLimit,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := &recordingDispatcher{}
			runtime := newEventRuntime(t, tt.stub, recorder)
			if tt.wantReason == execution.ReasonLoopLimit {
				runtime.config.MaxIterations = 1
			}
			_ = baseResponse

			_, err := runtime.ReplyWithTools(context.Background(), request)
			if err == nil {
				t.Fatal("expected error")
			}

			failed := eventsOf[execution.RunFailed](recorder.snapshot())
			if len(failed) != 1 {
				t.Fatalf("run failed events: %d, want 1", len(failed))
			}
			if failed[0].Reason != tt.wantReason {
				t.Fatalf("reason = %q, want %q", failed[0].Reason, tt.wantReason)
			}
			if failed[0].RunID != "run-abc" {
				t.Fatalf("run context: %+v", failed[0].RunContext)
			}
		})
	}
}

func TestReplyWithToolsGeneratesRunIDWhenAbsent(t *testing.T) {
	recorder := &recordingDispatcher{}
	stub := &stubLLM{responses: []llm.Response{
		{Message: llm.TextMessage(llm.RoleAssistant, "hi")},
	}}
	runtime := newEventRuntime(t, stub, recorder)

	request := eventRequest()
	request.RunID = ""
	if _, err := runtime.ReplyWithTools(context.Background(), request); err != nil {
		t.Fatalf("ReplyWithTools() error = %v", err)
	}

	started := eventsOf[execution.RunStarted](recorder.snapshot())
	if len(started) != 1 || started[0].RunID == "" || started[0].RunID == "run-abc" {
		t.Fatalf("generated run id: %+v", started)
	}
}

func TestReplyWithToolsWithoutDispatcherRunsSilently(t *testing.T) {
	stub := &stubLLM{responses: []llm.Response{
		{Message: llm.TextMessage(llm.RoleAssistant, "quiet")},
	}}
	runtime := newEventRuntime(t, stub, nil)

	result, err := runtime.ReplyWithTools(context.Background(), eventRequest())
	if err != nil {
		t.Fatalf("ReplyWithTools() error = %v", err)
	}
	if result.Reply != "quiet" || len(result.ToolCalls) != 0 {
		t.Fatalf("unexpected result: %+v", result)
	}
}
