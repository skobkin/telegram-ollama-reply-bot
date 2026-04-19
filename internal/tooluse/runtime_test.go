package tooluse

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"telegram-ollama-reply-bot/internal/content/extractor"
	"telegram-ollama-reply-bot/internal/llm"
	"telegram-ollama-reply-bot/internal/state"
	"telegram-ollama-reply-bot/internal/state/memory"

	tg "github.com/mymmrac/telego"
)

type stubLLM struct {
	buildErr        error
	generateErr     error
	fallbackReply   string
	fallbackUsage   *llm.TokenUsage
	buildCount      int
	fallbackCount   int
	responses       []llm.Response
	lastBuildPolicy string
	lastBuildTools  []llm.ToolDefinition
	lastBuildExtras []llm.Message
	lastGenerateReq llm.Request
}

func (s *stubLLM) BuildToolUseRequest(_ context.Context, _ llm.PromptScope, _ llm.ChatReplyContext, tools []llm.ToolDefinition, toolPolicy string, extraMessages []llm.Message) (llm.Request, error) {
	s.buildCount++
	s.lastBuildPolicy = toolPolicy
	s.lastBuildTools = append([]llm.ToolDefinition(nil), tools...)
	s.lastBuildExtras = append([]llm.Message(nil), extraMessages...)
	if s.buildErr != nil {
		return llm.Request{}, s.buildErr
	}

	return llm.Request{Feature: llm.FeatureToolUse, Tools: tools, Messages: extraMessages}, nil
}

func (s *stubLLM) Generate(_ context.Context, req llm.Request) (llm.Response, error) {
	s.lastGenerateReq = req
	if s.generateErr != nil {
		return llm.Response{}, s.generateErr
	}
	if len(s.responses) == 0 {
		return llm.Response{}, nil
	}

	resp := s.responses[0]
	s.responses = s.responses[1:]

	return resp, nil
}

func (s *stubLLM) HandleChatMessage(context.Context, llm.PromptScope, llm.ChatReplyContext) (string, *llm.TokenUsage, error) {
	s.fallbackCount++

	return s.fallbackReply, s.fallbackUsage, nil
}

type stubExtractor struct {
	article extractor.Article
	err     error
}

func (s *stubExtractor) GetArticleFromURL(context.Context, string) (extractor.Article, error) {
	return s.article, s.err
}

type stubPollSender struct {
	lastParams *tg.SendPollParams
	message    *tg.Message
	err        error
}

func (s *stubPollSender) SendPoll(_ context.Context, params *tg.SendPollParams) (*tg.Message, error) {
	s.lastParams = params

	return s.message, s.err
}

func TestRuntimeFallsBackToChatWhenToolUseGenerateFailsImmediately(t *testing.T) {
	runtime := New(
		&stubLLM{generateErr: errors.New("boom"), fallbackReply: "fallback", fallbackUsage: &llm.TokenUsage{TotalTokens: 5}},
		memory.New(memory.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil))).Conversations(),
		&stubExtractor{},
		&stubPollSender{},
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		Config{MaxIterations: 6},
	)

	reply, usage, err := runtime.HandleChatMessage(context.Background(), ChatRequest{
		Scope:        state.ConversationScope{ChatID: 1},
		PromptScope:  llm.PromptScope{ChatID: 1},
		ReplyContext: llm.ChatReplyContext{UserMessage: llm.TextMessage(llm.RoleUser, "hi")},
	})
	if err != nil {
		t.Fatalf("HandleChatMessage() error = %v", err)
	}
	if reply != "fallback" {
		t.Fatalf("unexpected fallback reply: %q", reply)
	}
	if usage == nil || usage.TotalTokens != 5 {
		t.Fatalf("unexpected fallback usage: %+v", usage)
	}
}

func TestRuntimeExecutesToolCallsAndReturnsFinalReply(t *testing.T) {
	store := memory.New(memory.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil))).Conversations()
	store.AppendMessage(state.ConversationScope{ChatID: 1}, state.Message{Name: "alice", Text: "meeting tomorrow", MessageID: 10})

	stub := &stubLLM{
		responses: []llm.Response{
			{
				Message: llm.Message{
					Role: llm.RoleAssistant,
					ToolCalls: []llm.ToolCall{
						{ID: "call-1", Name: "search_recent_history", Arguments: json.RawMessage(`{"query":"meeting","limit":1}`)},
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
		&stubPollSender{},
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		Config{MaxIterations: 6},
	)

	reply, usage, err := runtime.HandleChatMessage(context.Background(), ChatRequest{
		Scope:          state.ConversationScope{ChatID: 1},
		PromptScope:    llm.PromptScope{ChatID: 1},
		ReplyContext:   llm.ChatReplyContext{UserMessage: llm.TextMessage(llm.RoleUser, "what was planned?")},
		RequestMessage: state.Message{FromID: 11},
	})
	if err != nil {
		t.Fatalf("HandleChatMessage() error = %v", err)
	}
	if reply != "Found it" {
		t.Fatalf("unexpected reply: %q", reply)
	}
	if usage == nil || usage.TotalTokens != 7 {
		t.Fatalf("unexpected usage: %+v", usage)
	}
	if len(stub.lastBuildExtras) != 2 {
		t.Fatalf("expected assistant tool-call message and tool result, got %d extras", len(stub.lastBuildExtras))
	}
	if got := stub.lastBuildExtras[1].Text(); !strings.Contains(got, `"status":"ok"`) {
		t.Fatalf("unexpected tool result payload: %q", got)
	}
	if !strings.Contains(stub.lastBuildPolicy, "Explicit-request-only tools") || !strings.Contains(stub.lastBuildPolicy, "Discretionary tools") {
		t.Fatalf("unexpected tool policy: %q", stub.lastBuildPolicy)
	}
}

func TestRuntimeRespectsConfiguredIterationLimit(t *testing.T) {
	stub := &stubLLM{
		responses: []llm.Response{
			{Message: llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "1", Name: "get_conversation_summary", Arguments: json.RawMessage(`{}`)}}}},
			{Message: llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "2", Name: "get_conversation_summary", Arguments: json.RawMessage(`{}`)}}}},
		},
	}
	runtime := New(
		stub,
		memory.New(memory.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil))).Conversations(),
		&stubExtractor{},
		&stubPollSender{},
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		Config{MaxIterations: 1},
	)

	_, _, err := runtime.HandleChatMessage(context.Background(), ChatRequest{
		Scope:        state.ConversationScope{ChatID: 1},
		PromptScope:  llm.PromptScope{ChatID: 1},
		ReplyContext: llm.ChatReplyContext{UserMessage: llm.TextMessage(llm.RoleUser, "hi")},
	})
	if !errors.Is(err, ErrToolLoopLimitReached) {
		t.Fatalf("expected ErrToolLoopLimitReached, got %v", err)
	}
}

func TestCreatePollUsesCurrentTopic(t *testing.T) {
	sender := &stubPollSender{message: &tg.Message{MessageID: 77}}
	runtime := New(
		&stubLLM{},
		memory.New(memory.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil))).Conversations(),
		&stubExtractor{},
		sender,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		Config{MaxIterations: 6},
	)

	definition, ok := runtime.registry.Lookup("create_poll")
	if !ok {
		t.Fatal("create_poll not registered")
	}

	result, err := definition.Handler(context.Background(), CallContext{Scope: state.ConversationScope{ChatID: 1, TopicID: 42}}, json.RawMessage(`{"question":"Q?","options":["A","B"]}`))
	if err != nil {
		t.Fatalf("create_poll handler error = %v", err)
	}
	if result.Status != "ok" {
		t.Fatalf("unexpected result: %+v", result)
	}
	if sender.lastParams == nil || sender.lastParams.MessageThreadID != 42 {
		t.Fatalf("expected poll to target topic 42, got %+v", sender.lastParams)
	}
}

func TestReminderStubsAreRegisteredByDefault(t *testing.T) {
	runtime := New(
		&stubLLM{},
		memory.New(memory.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil))).Conversations(),
		&stubExtractor{},
		&stubPollSender{},
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		Config{MaxIterations: 6},
	)

	names := make([]string, 0, len(runtime.registry.DefaultDefinitions()))
	for _, definition := range runtime.registry.DefaultDefinitions() {
		names = append(names, definition.Name)
	}

	for _, required := range []string{"list_chat_schedule", "add_schedule_item", "remove_schedule_item"} {
		if !slices.Contains(names, required) {
			t.Fatalf("expected %s in default tool set, got %v", required, names)
		}
	}
}
