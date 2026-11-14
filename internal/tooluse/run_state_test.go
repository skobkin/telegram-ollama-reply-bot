package tooluse

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"

	"telegram-ollama-reply-bot/internal/execution"
	"telegram-ollama-reply-bot/internal/llm"
	"telegram-ollama-reply-bot/internal/state"
	"telegram-ollama-reply-bot/internal/state/memory"
)

// TestToolCallContextCarriesEffectiveRunIDAndMediaGroup pins the per-run
// context contract: every tool call of one run sees the same effective run ID
// (generated when the caller omitted it) and the same per-run state pointer,
// while a later run — even through the same runtime — sees a different run ID
// and a fresh state pointer, so run-scoped guards can never leak across runs.
func TestToolCallContextCarriesEffectiveRunIDAndMediaGroup(t *testing.T) {
	store := memory.New(memory.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil))).Conversations()

	var captured []CallContext
	probe := Definition{
		Name:             "run_probe",
		Description:      "Test probe capturing its call context.",
		Parameters:       json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
		InvocationPolicy: InvocationPolicyDiscretionary,
		Visibility:       VisibilityStandard,
		Handler: func(_ context.Context, callCtx CallContext, _ json.RawMessage) (toolResult, error) {
			captured = append(captured, callCtx)

			return toolResult{Status: execution.StatusOK, Summary: "probed"}, nil
		},
	}

	stub := &stubLLM{
		responses: []llm.Response{
			// First run: two probe calls, then the final text.
			{Message: llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{
				{ID: "call-1", Name: "run_probe", Arguments: json.RawMessage(`{}`)},
			}}},
			{Message: llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{
				{ID: "call-2", Name: "run_probe", Arguments: json.RawMessage(`{}`)},
			}}},
			{Message: llm.TextMessage(llm.RoleAssistant, "done")},
			// Second run: the same script again.
			{Message: llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{
				{ID: "call-3", Name: "run_probe", Arguments: json.RawMessage(`{}`)},
			}}},
			{Message: llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{
				{ID: "call-4", Name: "run_probe", Arguments: json.RawMessage(`{}`)},
			}}},
			{Message: llm.TextMessage(llm.RoleAssistant, "done again")},
		},
	}
	runtime := New(
		stub,
		store,
		&stubExtractor{},
		nil,
		&stubActionSender{},
		nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		Config{MaxIterations: 6},
	)
	runtime.registry.Register(probe)

	var startedRunIDs []string
	recorder := &recordingDispatcher{}
	runtime.config.Dispatcher = recorder

	result, err := runtime.ReplyWithTools(context.Background(), ChatRequest{
		Scope:                 state.ConversationScope{ChatID: 1},
		PromptScope:           llm.PromptScope{ChatID: 1},
		ReplyContext:          llm.ChatReplyContext{UserMessage: llm.TextMessage(llm.RoleUser, "hi")},
		RequestMessage:        state.Message{FromID: 11, MessageID: 42},
		RequesterMediaGroupID: "album-1",
	})
	if err != nil {
		t.Fatalf("first run error = %v", err)
	}
	if result.Reply != "done" {
		t.Fatalf("unexpected first run reply: %q", result.Reply)
	}

	for _, event := range eventsOf[execution.RunStarted](recorder.events) {
		startedRunIDs = append(startedRunIDs, event.RunID)
	}
	if len(startedRunIDs) != 1 {
		t.Fatalf("expected exactly one RunStarted, got %d", len(startedRunIDs))
	}

	if len(captured) != 2 {
		t.Fatalf("expected two probe calls in the first run, got %d", len(captured))
	}
	first, second := captured[0], captured[1]

	// Effective run ID: generated when omitted, identical for every call of
	// the run, and equal to the run lifecycle events' correlation ID.
	if first.RunID == "" {
		t.Fatal("CallContext.RunID must carry the effective run ID, not the logging RequestID")
	}
	if first.RunID != second.RunID {
		t.Fatalf("calls of one run must share the run ID: %q vs %q", first.RunID, second.RunID)
	}
	if first.RunID != startedRunIDs[0] {
		t.Fatalf("CallContext.RunID %q must equal RunStarted.RunID %q", first.RunID, startedRunIDs[0])
	}
	if first.RequestID == first.RunID {
		t.Fatal("RunID must be the run correlation ID, not the logging RequestID")
	}

	// Per-run state: shared by the run's calls, carrying the media-group flag
	// and the request scope/requester identity.
	if first.run == nil || second.run == nil {
		t.Fatal("CallContext.run must be set by the runtime")
	}
	if first.run != second.run {
		t.Fatal("calls of one run must share the per-run state pointer")
	}
	if first.RequesterMediaGroupID != "album-1" || second.RequesterMediaGroupID != "album-1" {
		t.Fatalf("media-group flag must propagate to every call: %q / %q", first.RequesterMediaGroupID, second.RequesterMediaGroupID)
	}
	if first.Scope.ChatID != 1 || first.Requester.MessageID != 42 {
		t.Fatalf("unexpected scope/requester in call context: %+v", first)
	}

	// A second run through the same runtime gets a new run ID and fresh
	// per-run state: nothing leaks across runs.
	captured = nil
	recorder.events = nil
	result, err = runtime.ReplyWithTools(context.Background(), ChatRequest{
		Scope:          state.ConversationScope{ChatID: 1},
		PromptScope:    llm.PromptScope{ChatID: 1},
		ReplyContext:   llm.ChatReplyContext{UserMessage: llm.TextMessage(llm.RoleUser, "again")},
		RequestMessage: state.Message{FromID: 11, MessageID: 43},
		RunID:          "explicit-run",
	})
	if err != nil {
		t.Fatalf("second run error = %v", err)
	}
	if result.Reply != "done again" {
		t.Fatalf("unexpected second run reply: %q", result.Reply)
	}

	for _, event := range eventsOf[execution.RunStarted](recorder.events) {
		startedRunIDs = append(startedRunIDs, event.RunID)
	}
	if len(startedRunIDs) != 2 || startedRunIDs[1] != "explicit-run" {
		t.Fatalf("expected the explicit run ID on the second run, got %v", startedRunIDs)
	}
	if len(captured) != 2 {
		t.Fatalf("expected two probe calls in the second run, got %d", len(captured))
	}
	if captured[0].RunID != "explicit-run" || captured[1].RunID != "explicit-run" {
		t.Fatalf("second run calls must carry the explicit run ID: %q / %q", captured[0].RunID, captured[1].RunID)
	}
	if captured[0].run == first.run {
		t.Fatal("a new run must get fresh per-run state, not reuse the previous run's")
	}
	if captured[0].RequesterMediaGroupID != "" {
		t.Fatalf("media-group flag must not leak across runs, got %q", captured[0].RequesterMediaGroupID)
	}
}
