package tooluse

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"

	"telegram-ollama-reply-bot/internal/llm"
	"telegram-ollama-reply-bot/internal/state"
	"telegram-ollama-reply-bot/internal/state/memory"
)

// TestReplyWithToolsThreadsTurnModelAcrossIterations pins the turn-model
// contract: the runtime passes the ChatRequest's TurnConfig.GenerationModel
// to every BuildChatToolRequest call of the run, so a multi-iteration turn
// keeps one model on both the wire request and the {{.Model}} prompt
// placeholder no matter how many tool loops it takes.
func TestReplyWithToolsThreadsTurnModelAcrossIterations(t *testing.T) {
	store := memory.New(memory.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil))).Conversations()
	store.AppendMessage(state.ConversationScope{ChatID: 1}, state.Message{Name: "alice", Text: "meeting tomorrow", MessageID: 10})

	stub := &stubLLM{
		responses: []llm.Response{
			{
				Message: llm.Message{
					Role: llm.RoleAssistant,
					ToolCalls: []llm.ToolCall{
						{ID: "call-1", Name: "search_history", Arguments: json.RawMessage(`{"keywords":["meeting"],"match_mode":"all","limit":1}`)},
					},
				},
				Usage: llm.TokenUsage{TotalTokens: 3},
			},
			{
				Message: llm.TextMessage(llm.RoleAssistant, "Found it"),
				Usage:   llm.TokenUsage{TotalTokens: 4},
			},
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

	result, err := runtime.ReplyWithTools(context.Background(), ChatRequest{
		Scope:          state.ConversationScope{ChatID: 1},
		PromptScope:    llm.PromptScope{ChatID: 1},
		ReplyContext:   llm.ChatReplyContext{UserMessage: llm.TextMessage(llm.RoleUser, "what was planned?")},
		RequestMessage: state.Message{FromID: 11},
		TurnConfig: llm.TurnConfig{
			GenerationModel: "turn-model",
			SystemOneModel:  "decision-model",
		},
	})
	if err != nil {
		t.Fatalf("ReplyWithTools() error = %v", err)
	}
	if result.Reply != "Found it" {
		t.Fatalf("unexpected reply: %q", result.Reply)
	}

	if len(stub.buildModels) != 2 {
		t.Fatalf("expected two request builds, got %d", len(stub.buildModels))
	}
	for i, model := range stub.buildModels {
		if model != "turn-model" {
			t.Fatalf("iteration %d: expected the turn model threaded to BuildChatToolRequest, got %q", i, model)
		}
	}
	for i, model := range stub.generateModels {
		if model != "turn-model" {
			t.Fatalf("iteration %d: expected the turn model on the wire request, got %q", i, model)
		}
	}
	if stub.lastGenerateReq.Model != "turn-model" {
		t.Fatalf("expected the final wire request to carry the turn model, got %q", stub.lastGenerateReq.Model)
	}
}

// TestReplyWithToolsUnsetTurnConfigKeepsEnvironmentFallback pins the unset
// semantics for callers that do not resolve runtime settings: an empty
// TurnConfig threads an empty model, which leaves the wire model to the
// environment route exactly as before runtime overrides existed.
func TestReplyWithToolsUnsetTurnConfigKeepsEnvironmentFallback(t *testing.T) {
	store := memory.New(memory.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil))).Conversations()

	stub := &stubLLM{
		responses: []llm.Response{
			{Message: llm.TextMessage(llm.RoleAssistant, "done")},
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

	if _, err := runtime.ReplyWithTools(context.Background(), ChatRequest{
		Scope:          state.ConversationScope{ChatID: 1},
		PromptScope:    llm.PromptScope{ChatID: 1},
		ReplyContext:   llm.ChatReplyContext{UserMessage: llm.TextMessage(llm.RoleUser, "hi")},
		RequestMessage: state.Message{FromID: 11},
	}); err != nil {
		t.Fatalf("ReplyWithTools() error = %v", err)
	}

	if len(stub.buildModels) != 1 || stub.buildModels[0] != "" {
		t.Fatalf("expected an empty threaded model, got %v", stub.buildModels)
	}
	if len(stub.generateModels) != 1 || stub.generateModels[0] != "" {
		t.Fatalf("expected an unset wire model, got %v", stub.generateModels)
	}
}
