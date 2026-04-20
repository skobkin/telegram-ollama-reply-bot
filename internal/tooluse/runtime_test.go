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
	"time"

	"telegram-ollama-reply-bot/internal/content/extractor"
	"telegram-ollama-reply-bot/internal/llm"
	"telegram-ollama-reply-bot/internal/state"
	"telegram-ollama-reply-bot/internal/state/memory"

	tg "github.com/mymmrac/telego"
)

type stubLLM struct {
	buildErr        error
	generateErr     error
	buildCount      int
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

func TestRuntimeReturnsUnavailableWhenToolUseGenerateFailsImmediately(t *testing.T) {
	runtime := New(
		&stubLLM{generateErr: errors.New("boom")},
		memory.New(memory.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil))).Conversations(),
		&stubExtractor{},
		&stubPollSender{},
		nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		Config{MaxIterations: 6},
	)

	reply, usage, err := runtime.ReplyWithTools(context.Background(), ChatRequest{
		Scope:        state.ConversationScope{ChatID: 1},
		PromptScope:  llm.PromptScope{ChatID: 1},
		ReplyContext: llm.ChatReplyContext{UserMessage: llm.TextMessage(llm.RoleUser, "hi")},
	})
	if !errors.Is(err, ErrToolUseUnavailable) {
		t.Fatalf("expected ErrToolUseUnavailable, got %v", err)
	}
	if reply != "" {
		t.Fatalf("unexpected reply: %q", reply)
	}
	if usage != nil {
		t.Fatalf("unexpected usage: %+v", usage)
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
		nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		Config{MaxIterations: 6},
	)

	reply, usage, err := runtime.ReplyWithTools(context.Background(), ChatRequest{
		Scope:          state.ConversationScope{ChatID: 1},
		PromptScope:    llm.PromptScope{ChatID: 1},
		ReplyContext:   llm.ChatReplyContext{UserMessage: llm.TextMessage(llm.RoleUser, "what was planned?")},
		RequestMessage: state.Message{FromID: 11},
	})
	if err != nil {
		t.Fatalf("ReplyWithTools() error = %v", err)
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
		nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		Config{MaxIterations: 1},
	)

	_, _, err := runtime.ReplyWithTools(context.Background(), ChatRequest{
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
		nil,
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

func TestReminderToolsAreRegisteredByDefault(t *testing.T) {
	runtime := New(
		&stubLLM{},
		memory.New(memory.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil))).Conversations(),
		&stubExtractor{},
		&stubPollSender{},
		nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		Config{MaxIterations: 6},
	)

	names := make([]string, 0, len(runtime.registry.DefaultDefinitions()))
	for _, definition := range runtime.registry.DefaultDefinitions() {
		names = append(names, definition.Name)
	}

	for _, required := range []string{"get_current_time", "list_chat_schedule", "add_schedule_item", "remove_schedule_item"} {
		if !slices.Contains(names, required) {
			t.Fatalf("expected %s in default tool set, got %v", required, names)
		}
	}
}

func TestNewToolsAreRegisteredByDefault(t *testing.T) {
	runtime := New(
		&stubLLM{},
		memory.New(memory.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil))).Conversations(),
		&stubExtractor{},
		&stubPollSender{},
		nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		Config{MaxIterations: 6},
	)

	names := make([]string, 0, len(runtime.registry.DefaultDefinitions()))
	for _, definition := range runtime.registry.DefaultDefinitions() {
		names = append(names, definition.Name)
	}

	for _, required := range []string{"list_recent_links", "convert_timezone", "shift_datetime"} {
		if !slices.Contains(names, required) {
			t.Fatalf("expected %s in default tool set, got %v", required, names)
		}
	}
}

func TestListRecentLinksUsesScopeAndDeduplicates(t *testing.T) {
	store := memory.New(memory.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil))).Conversations()
	scope := state.ConversationScope{ChatID: 1, TopicID: 42}
	otherScope := state.ConversationScope{ChatID: 1, TopicID: 99}

	store.AppendMessage(scope, state.Message{
		Name:      "alice",
		Username:  "alice",
		Text:      "first https://example.com/a",
		MessageID: 1,
		FromID:    10,
		CreatedAt: time.Date(2026, 4, 20, 10, 0, 0, 0, time.UTC),
	})
	store.AppendMessage(scope, state.Message{
		Name:      "bob",
		Username:  "bob",
		Text:      "again https://example.com/a and https://example.com/b",
		MessageID: 2,
		FromID:    11,
		CreatedAt: time.Date(2026, 4, 20, 11, 0, 0, 0, time.UTC),
	})
	store.AppendMessage(otherScope, state.Message{
		Name:      "mallory",
		Text:      "ignore https://example.com/c",
		MessageID: 3,
		FromID:    12,
		CreatedAt: time.Date(2026, 4, 20, 12, 0, 0, 0, time.UTC),
	})

	runtime := New(
		&stubLLM{},
		store,
		&stubExtractor{},
		&stubPollSender{},
		nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		Config{MaxIterations: 6},
	)

	definition, ok := runtime.registry.Lookup("list_recent_links")
	if !ok {
		t.Fatal("list_recent_links not registered")
	}

	result, err := definition.Handler(context.Background(), CallContext{Scope: scope}, json.RawMessage(`{"limit":5}`))
	if err != nil {
		t.Fatalf("list_recent_links handler error = %v", err)
	}
	if result.Status != "ok" {
		t.Fatalf("unexpected result: %+v", result)
	}

	data, ok := result.Data.(map[string]any)
	if !ok {
		t.Fatalf("unexpected data type: %T", result.Data)
	}

	links, ok := data["links"].([]map[string]any)
	if !ok {
		t.Fatalf("unexpected links type: %T", data["links"])
	}
	if len(links) != 2 {
		t.Fatalf("expected 2 unique links, got %d", len(links))
	}
	if got := links[0]["url"]; got != "https://example.com/a" && got != "https://example.com/b" {
		t.Fatalf("unexpected first link url: %v", got)
	}
	if got := links[0]["message_id"]; got != 2 {
		t.Fatalf("expected newest scope message first, got %v", got)
	}
}

func TestListRecentLinksReturnsEmptyWhenNoLinksFound(t *testing.T) {
	store := memory.New(memory.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil))).Conversations()
	scope := state.ConversationScope{ChatID: 1}
	store.AppendMessage(scope, state.Message{Name: "alice", Text: "no url here"})

	runtime := New(
		&stubLLM{},
		store,
		&stubExtractor{},
		&stubPollSender{},
		nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		Config{MaxIterations: 6},
	)

	definition, _ := runtime.registry.Lookup("list_recent_links")
	result, err := definition.Handler(context.Background(), CallContext{Scope: scope}, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("list_recent_links handler error = %v", err)
	}
	if result.Status != "empty" {
		t.Fatalf("expected empty status, got %+v", result)
	}
}

func TestConvertTimezoneHandler(t *testing.T) {
	result, err := convertTimezoneHandler(context.Background(), CallContext{}, json.RawMessage(`{"timestamp":"2026-04-20T12:00:00+03:00","target_timezones":["UTC","America/New_York"]}`))
	if err != nil {
		t.Fatalf("convertTimezoneHandler() error = %v", err)
	}
	if result.Status != "ok" {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func TestConvertTimezoneHandlerRejectsInvalidTimezone(t *testing.T) {
	_, err := convertTimezoneHandler(context.Background(), CallContext{}, json.RawMessage(`{"timestamp":"2026-04-20T12:00:00+03:00","target_timezones":["Nope/Nowhere"]}`))
	if err == nil {
		t.Fatal("expected invalid timezone error")
	}
}

func TestShiftDateTimeHandler(t *testing.T) {
	result, err := shiftDateTimeHandler(context.Background(), CallContext{}, json.RawMessage(`{"timestamp":"2026-04-20T12:00:00+03:00","days":1,"hours":-2,"minutes":30}`))
	if err != nil {
		t.Fatalf("shiftDateTimeHandler() error = %v", err)
	}
	if result.Status != "ok" {
		t.Fatalf("unexpected result: %+v", result)
	}

	data, ok := result.Data.(map[string]any)
	if !ok {
		t.Fatalf("unexpected data type: %T", result.Data)
	}
	if got := data["shifted_timestamp_rfc3339"]; got != "2026-04-21T10:30:00+03:00" {
		t.Fatalf("unexpected shifted timestamp: %v", got)
	}
}

func TestShiftDateTimeHandlerRejectsZeroDelta(t *testing.T) {
	_, err := shiftDateTimeHandler(context.Background(), CallContext{}, json.RawMessage(`{"timestamp":"2026-04-20T12:00:00+03:00"}`))
	if err == nil {
		t.Fatal("expected zero-delta error")
	}
}
