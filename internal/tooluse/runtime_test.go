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
	"telegram-ollama-reply-bot/internal/content/search"
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

type stubActionSender struct {
	lastPollParams *tg.SendPollParams
	lastDiceParams *tg.SendDiceParams
	message        *tg.Message
	diceMessage    *tg.Message
	err            error
}

func (s *stubActionSender) SendPoll(_ context.Context, params *tg.SendPollParams) (*tg.Message, error) {
	s.lastPollParams = params

	return s.message, s.err
}

func (s *stubActionSender) SendDice(_ context.Context, params *tg.SendDiceParams) (*tg.Message, error) {
	s.lastDiceParams = params
	if s.diceMessage != nil {
		return s.diceMessage, s.err
	}

	return s.message, s.err
}

type stubSearcher struct {
	result search.Result
	err    error
}

func (s *stubSearcher) Search(context.Context, search.Request) (search.Result, error) {
	return s.result, s.err
}

func TestRuntimeReturnsUnavailableWhenToolUseGenerateFailsImmediately(t *testing.T) {
	runtime := New(
		&stubLLM{generateErr: errors.New("boom")},
		memory.New(memory.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil))).Conversations(),
		&stubExtractor{},
		nil,
		&stubActionSender{},
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

func TestRuntimeAllowsEmptyFinalReplyAfterSuccessfulSideEffectTool(t *testing.T) {
	stub := &stubLLM{
		responses: []llm.Response{
			{
				Message: llm.Message{
					Role: llm.RoleAssistant,
					ToolCalls: []llm.ToolCall{
						{ID: "call-1", Name: "send_dice", Arguments: json.RawMessage(`{}`)},
					},
				},
				Usage: llm.TokenUsage{TotalTokens: 3},
			},
			{
				Message: llm.Message{Role: llm.RoleAssistant},
				Usage:   llm.TokenUsage{TotalTokens: 4},
			},
		},
	}
	runtime := New(
		stub,
		memory.New(memory.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil))).Conversations(),
		&stubExtractor{},
		nil,
		&stubActionSender{diceMessage: &tg.Message{MessageID: 55, Dice: &tg.Dice{Emoji: "🎲", Value: 6}}},
		nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		Config{MaxIterations: 6},
	)

	reply, usage, err := runtime.ReplyWithTools(context.Background(), ChatRequest{
		Scope:        state.ConversationScope{ChatID: 1},
		PromptScope:  llm.PromptScope{ChatID: 1},
		ReplyContext: llm.ChatReplyContext{UserMessage: llm.TextMessage(llm.RoleUser, "roll a dice")},
	})
	if err != nil {
		t.Fatalf("ReplyWithTools() error = %v", err)
	}
	if reply != "" {
		t.Fatalf("expected empty reply, got %q", reply)
	}
	if usage == nil || usage.TotalTokens != 7 {
		t.Fatalf("unexpected usage: %+v", usage)
	}
}

func TestRuntimeRejectsEmptyFinalReplyAfterNonSideEffectTool(t *testing.T) {
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
				Message: llm.Message{Role: llm.RoleAssistant},
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

	reply, usage, err := runtime.ReplyWithTools(context.Background(), ChatRequest{
		Scope:          state.ConversationScope{ChatID: 1},
		PromptScope:    llm.PromptScope{ChatID: 1},
		ReplyContext:   llm.ChatReplyContext{UserMessage: llm.TextMessage(llm.RoleUser, "what was planned?")},
		RequestMessage: state.Message{FromID: 11},
	})
	if !errors.Is(err, llm.ErrNoChoices) {
		t.Fatalf("expected ErrNoChoices, got %v", err)
	}
	if reply != "" {
		t.Fatalf("expected empty reply, got %q", reply)
	}
	if usage == nil || usage.TotalTokens != 7 {
		t.Fatalf("unexpected usage: %+v", usage)
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
		nil,
		&stubActionSender{},
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

func TestGetConversationSummaryReturnsCachedSummaryWithFullHistoryPresent(t *testing.T) {
	store := memory.New(memory.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil))).Conversations()
	scope := state.ConversationScope{ChatID: 1, TopicID: 7}
	for i, text := range []string{"one", "two", "three", "four"} {
		store.AppendMessage(scope, state.Message{
			Name:      "alice",
			Text:      text,
			MessageID: i + 1,
			CreatedAt: time.Now().UTC(),
		})
	}
	store.SetEarlierSummary(scope, "summary of one and two", 2)

	runtime := New(
		&stubLLM{},
		store,
		&stubExtractor{},
		nil,
		&stubActionSender{},
		nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		Config{MaxIterations: 6},
	)

	definition, ok := runtime.registry.Lookup("get_conversation_summary")
	if !ok {
		t.Fatal("get_conversation_summary not registered")
	}

	result, err := definition.Handler(context.Background(), CallContext{Scope: scope}, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("get_conversation_summary handler error = %v", err)
	}
	if result.Status != "ok" {
		t.Fatalf("unexpected result: %+v", result)
	}

	data, ok := result.Data.(map[string]any)
	if !ok {
		t.Fatalf("unexpected data type: %T", result.Data)
	}
	if got := data["summary"]; got != "summary of one and two" {
		t.Fatalf("unexpected summary: %v", got)
	}
	if got := len(store.Snapshot(scope).Messages); got != 4 {
		t.Fatalf("expected full raw history to remain available, got %d messages", got)
	}
}

func TestSendPollUsesCurrentTopic(t *testing.T) {
	sender := &stubActionSender{message: &tg.Message{MessageID: 77}}
	runtime := New(
		&stubLLM{},
		memory.New(memory.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil))).Conversations(),
		&stubExtractor{},
		nil,
		sender,
		nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		Config{MaxIterations: 6},
	)

	definition, ok := runtime.registry.Lookup("send_poll")
	if !ok {
		t.Fatal("send_poll not registered")
	}

	result, err := definition.Handler(context.Background(), CallContext{Scope: state.ConversationScope{ChatID: 1, TopicID: 42}}, json.RawMessage(`{"question":"Q?","options":["A","B"]}`))
	if err != nil {
		t.Fatalf("send_poll handler error = %v", err)
	}
	if result.Status != "ok" {
		t.Fatalf("unexpected result: %+v", result)
	}
	if sender.lastPollParams == nil || sender.lastPollParams.MessageThreadID != 42 {
		t.Fatalf("expected poll to target topic 42, got %+v", sender.lastPollParams)
	}
}

func TestSendQuizUsesCurrentTopicAndCorrectOption(t *testing.T) {
	sender := &stubActionSender{message: &tg.Message{MessageID: 88}}
	runtime := New(
		&stubLLM{},
		memory.New(memory.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil))).Conversations(),
		&stubExtractor{},
		nil,
		sender,
		nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		Config{MaxIterations: 6},
	)

	definition, ok := runtime.registry.Lookup("send_quiz")
	if !ok {
		t.Fatal("send_quiz not registered")
	}

	result, err := definition.Handler(context.Background(), CallContext{Scope: state.ConversationScope{ChatID: 1, TopicID: 42}}, json.RawMessage(`{"question":"Q?","options":["A","B","C"],"correct_option_index":1,"explanation":"Because."}`))
	if err != nil {
		t.Fatalf("send_quiz handler error = %v", err)
	}
	if result.Status != "ok" {
		t.Fatalf("unexpected result: %+v", result)
	}
	if sender.lastPollParams == nil {
		t.Fatal("expected poll params to be captured")
	}
	if sender.lastPollParams.MessageThreadID != 42 {
		t.Fatalf("expected quiz to target topic 42, got %+v", sender.lastPollParams)
	}
	if sender.lastPollParams.Type != "quiz" {
		t.Fatalf("expected quiz poll type, got %q", sender.lastPollParams.Type)
	}
	if !slices.Equal(sender.lastPollParams.CorrectOptionIDs, []int{1}) {
		t.Fatalf("unexpected correct option ids: %v", sender.lastPollParams.CorrectOptionIDs)
	}
	if sender.lastPollParams.Explanation != "Because." {
		t.Fatalf("unexpected explanation: %q", sender.lastPollParams.Explanation)
	}
}

func TestSendQuizRejectsInvalidCorrectOptionIndex(t *testing.T) {
	runtime := New(
		&stubLLM{},
		memory.New(memory.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil))).Conversations(),
		&stubExtractor{},
		nil,
		&stubActionSender{},
		nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		Config{MaxIterations: 6},
	)

	definition, _ := runtime.registry.Lookup("send_quiz")
	_, err := definition.Handler(context.Background(), CallContext{Scope: state.ConversationScope{ChatID: 1}}, json.RawMessage(`{"question":"Q?","options":["A","B"],"correct_option_index":2}`))
	if err == nil || err.Error() != "correct_option_index must reference an existing option" {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestSendDiceUsesCurrentTopicAndReturnsValue(t *testing.T) {
	sender := &stubActionSender{diceMessage: &tg.Message{MessageID: 91, Dice: &tg.Dice{Emoji: "🎯", Value: 6}}}
	runtime := New(
		&stubLLM{},
		memory.New(memory.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil))).Conversations(),
		&stubExtractor{},
		nil,
		sender,
		nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		Config{MaxIterations: 6},
	)

	definition, ok := runtime.registry.Lookup("send_dice")
	if !ok {
		t.Fatal("send_dice not registered")
	}

	result, err := definition.Handler(context.Background(), CallContext{Scope: state.ConversationScope{ChatID: 1, TopicID: 42}}, json.RawMessage(`{"emoji":"🎯"}`))
	if err != nil {
		t.Fatalf("send_dice handler error = %v", err)
	}
	if result.Status != "ok" {
		t.Fatalf("unexpected result: %+v", result)
	}
	if sender.lastDiceParams == nil || sender.lastDiceParams.MessageThreadID != 42 {
		t.Fatalf("expected dice to target topic 42, got %+v", sender.lastDiceParams)
	}
	if sender.lastDiceParams.Emoji != "🎯" {
		t.Fatalf("unexpected dice emoji: %q", sender.lastDiceParams.Emoji)
	}

	data := result.Data.(map[string]any)
	if got := data["emoji"]; got != "🎯" {
		t.Fatalf("unexpected result emoji: %v", got)
	}
	if got := data["value"]; got != 6 {
		t.Fatalf("unexpected result value: %v", got)
	}
}

func TestSendDiceDefaultsAndRejectsUnsupportedEmoji(t *testing.T) {
	sender := &stubActionSender{diceMessage: &tg.Message{MessageID: 92}}
	runtime := New(
		&stubLLM{},
		memory.New(memory.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil))).Conversations(),
		&stubExtractor{},
		nil,
		sender,
		nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		Config{MaxIterations: 6},
	)

	definition, _ := runtime.registry.Lookup("send_dice")

	result, err := definition.Handler(context.Background(), CallContext{Scope: state.ConversationScope{ChatID: 1}}, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("send_dice default handler error = %v", err)
	}
	if result.Status != "ok" {
		t.Fatalf("unexpected result: %+v", result)
	}
	if sender.lastDiceParams == nil {
		t.Fatal("expected dice params to be captured")
	}
	if sender.lastDiceParams.Emoji != "" {
		t.Fatalf("expected empty emoji to rely on Telegram default, got %q", sender.lastDiceParams.Emoji)
	}

	_, err = definition.Handler(context.Background(), CallContext{Scope: state.ConversationScope{ChatID: 1}}, json.RawMessage(`{"emoji":"🎮"}`))
	if err == nil || err.Error() != "emoji must be one of 🎲, 🎯, 🏀, ⚽, 🎳, or 🎰" {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestReminderToolsAreRegisteredByDefault(t *testing.T) {
	runtime := New(
		&stubLLM{},
		memory.New(memory.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil))).Conversations(),
		&stubExtractor{},
		nil,
		&stubActionSender{},
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
		nil,
		&stubActionSender{},
		nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		Config{MaxIterations: 6},
	)

	names := make([]string, 0, len(runtime.registry.DefaultDefinitions()))
	for _, definition := range runtime.registry.DefaultDefinitions() {
		names = append(names, definition.Name)
	}

	for _, required := range []string{"list_recent_links", "datetime_math", "datetime_format", "get_chat_activity_window", "get_history_bounds", "get_message_thread_context", "search_history", "send_poll", "send_quiz", "send_dice"} {
		if !slices.Contains(names, required) {
			t.Fatalf("expected %s in default tool set, got %v", required, names)
		}
	}
	for _, obsolete := range []string{"convert_timezone", "shift_datetime"} {
		if slices.Contains(names, obsolete) {
			t.Fatalf("did not expect obsolete tool %s in default tool set, got %v", obsolete, names)
		}
	}
	if slices.Contains(names, "create_poll") {
		t.Fatalf("did not expect obsolete tool create_poll in default tool set, got %v", names)
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
		nil,
		&stubActionSender{},
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
		nil,
		&stubActionSender{},
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

func TestSearchHistorySupportsAnyAllAndFuzzyMatching(t *testing.T) {
	store := memory.New(memory.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil))).Conversations()
	scope := state.ConversationScope{ChatID: 1, TopicID: 9}
	base := time.Date(2026, 4, 20, 10, 0, 0, 0, time.UTC)

	store.AppendMessage(scope, state.Message{
		Name:      "alice",
		Text:      "Need to move my appointment to Friday",
		MessageID: 1,
		CreatedAt: base,
	})
	store.AppendMessage(scope, state.Message{
		Name:      "bob",
		Text:      "I wrote down the appaintments backlog typo on purpose",
		MessageID: 2,
		CreatedAt: base.Add(1 * time.Minute),
	})
	store.AppendMessage(scope, state.Message{
		Name:      "carol",
		Text:      "Friday groceries only",
		MessageID: 3,
		CreatedAt: base.Add(2 * time.Minute),
	})

	runtime := New(
		&stubLLM{},
		store,
		&stubExtractor{},
		nil,
		&stubActionSender{},
		nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		Config{MaxIterations: 6},
	)

	definition, _ := runtime.registry.Lookup("search_history")

	allResult, err := definition.Handler(context.Background(), CallContext{Scope: scope}, json.RawMessage(`{"keywords":["appointment","Friday"],"match_mode":"all","limit":20}`))
	if err != nil {
		t.Fatalf("search_history all handler error = %v", err)
	}
	if allResult.Status != "ok" {
		t.Fatalf("unexpected all result: %+v", allResult)
	}

	allData := allResult.Data.(map[string]any)
	allMatches, ok := allData["matches"].([]map[string]any)
	if !ok {
		t.Fatalf("unexpected all matches type: %T", allData["matches"])
	}
	if len(allMatches) != 1 {
		t.Fatalf("expected one all-match, got %d", len(allMatches))
	}
	if got := allMatches[0]["message_id"]; got != 1 {
		t.Fatalf("unexpected all-match message: %v", got)
	}
	if got := allMatches[0]["match_kind"]; got != "exact" {
		t.Fatalf("unexpected all-match kind: %v", got)
	}

	anyResult, err := definition.Handler(context.Background(), CallContext{Scope: scope}, json.RawMessage(`{"keywords":["appointment","Friday"],"match_mode":"any","limit":20}`))
	if err != nil {
		t.Fatalf("search_history any handler error = %v", err)
	}
	if anyResult.Status != "ok" {
		t.Fatalf("unexpected any result: %+v", anyResult)
	}

	anyData := anyResult.Data.(map[string]any)
	anyMatches, ok := anyData["matches"].([]map[string]any)
	if !ok {
		t.Fatalf("unexpected any matches type: %T", anyData["matches"])
	}
	if len(anyMatches) != 3 {
		t.Fatalf("expected three any-matches, got %d", len(anyMatches))
	}
	if got := anyMatches[0]["message_id"]; got != 1 {
		t.Fatalf("expected stronger exact match first, got %v", got)
	}
	if got := anyMatches[1]["message_id"]; got != 3 {
		t.Fatalf("expected newer exact single-keyword match second, got %v", got)
	}
	if got := anyMatches[2]["message_id"]; got != 2 {
		t.Fatalf("expected fuzzy typo match last, got %v", got)
	}
	if got := anyMatches[2]["match_kind"]; got != "fuzzy" {
		t.Fatalf("unexpected fuzzy match kind: %v", got)
	}
	if got := anyMatches[2]["score"]; got != 105 {
		t.Fatalf("unexpected fuzzy score: %v", got)
	}
}

func TestSearchHistoryLargeLimitAndValidation(t *testing.T) {
	store := memory.New(memory.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil))).Conversations()
	scope := state.ConversationScope{ChatID: 1}
	base := time.Date(2026, 4, 20, 10, 0, 0, 0, time.UTC)
	for i := 0; i < 25; i++ {
		store.AppendMessage(scope, state.Message{
			Name:      "alice",
			Text:      "reminder item",
			MessageID: i + 1,
			CreatedAt: base.Add(time.Duration(i) * time.Minute),
		})
	}

	runtime := New(
		&stubLLM{},
		store,
		&stubExtractor{},
		nil,
		&stubActionSender{},
		nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		Config{MaxIterations: 6},
	)

	definition, _ := runtime.registry.Lookup("search_history")

	result, err := definition.Handler(context.Background(), CallContext{Scope: scope}, json.RawMessage(`{"keywords":["reminder"],"match_mode":"all","limit":20}`))
	if err != nil {
		t.Fatalf("search_history large limit handler error = %v", err)
	}
	if result.Status != "ok" {
		t.Fatalf("unexpected result: %+v", result)
	}
	data := result.Data.(map[string]any)
	matches, ok := data["matches"].([]map[string]any)
	if !ok {
		t.Fatalf("unexpected matches type: %T", data["matches"])
	}
	if len(matches) != 20 {
		t.Fatalf("expected 20 matches, got %d", len(matches))
	}
	if got := matches[0]["message_id"]; got != 25 {
		t.Fatalf("expected newest result first on equal score, got %v", got)
	}

	invalidResult, err := definition.Handler(context.Background(), CallContext{Scope: scope}, json.RawMessage(`{"keywords":["reminder"],"match_mode":"nope"}`))
	if err != nil {
		t.Fatalf("search_history invalid mode handler error = %v", err)
	}
	assertToolError(t, invalidResult, "invalid_match_mode", "match_mode must be one of all, any")
}

func TestGetChatActivityWindowEmpty(t *testing.T) {
	runtime := New(
		&stubLLM{},
		memory.New(memory.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil))).Conversations(),
		&stubExtractor{},
		nil,
		&stubActionSender{},
		nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		Config{MaxIterations: 6},
	)

	definition, ok := runtime.registry.Lookup("get_chat_activity_window")
	if !ok {
		t.Fatal("get_chat_activity_window not registered")
	}

	result, err := definition.Handler(context.Background(), CallContext{Scope: state.ConversationScope{ChatID: 1, TopicID: 2}}, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("get_chat_activity_window handler error = %v", err)
	}
	if result.Status != "empty" {
		t.Fatalf("expected empty status, got %+v", result)
	}
}

func TestGetChatActivityWindowInsufficientData(t *testing.T) {
	store := memory.New(memory.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil))).Conversations()
	scope := state.ConversationScope{ChatID: 1}
	store.AppendMessage(scope, state.Message{
		Name:      "alice",
		Text:      "hello",
		MessageID: 1,
		CreatedAt: time.Date(2026, 4, 20, 10, 0, 0, 0, time.UTC),
	})

	runtime := New(
		&stubLLM{},
		store,
		&stubExtractor{},
		nil,
		&stubActionSender{},
		nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		Config{MaxIterations: 6},
	)

	definition, _ := runtime.registry.Lookup("get_chat_activity_window")
	result, err := definition.Handler(context.Background(), CallContext{Scope: scope}, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("get_chat_activity_window handler error = %v", err)
	}
	assertToolDataSubset(t, result, map[string]any{
		"message_count":        1,
		"window_span_seconds":  int64(0),
		"average_gap_seconds":  0.0,
		"messages_in_last_10m": 1,
		"messages_in_last_1h":  1,
		"burstiness":           "insufficient_data",
	})
}

func TestGetChatActivityWindowSteady(t *testing.T) {
	store := memory.New(memory.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil))).Conversations()
	scope := state.ConversationScope{ChatID: 1, TopicID: 5}
	base := time.Date(2026, 4, 20, 10, 0, 0, 0, time.UTC)
	for i := 0; i < 4; i++ {
		store.AppendMessage(scope, state.Message{
			Name:      "alice",
			Text:      "steady",
			MessageID: i + 1,
			CreatedAt: base.Add(time.Duration(i) * 5 * time.Minute),
		})
	}

	runtime := New(
		&stubLLM{},
		store,
		&stubExtractor{},
		nil,
		&stubActionSender{},
		nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		Config{MaxIterations: 6},
	)

	definition, _ := runtime.registry.Lookup("get_chat_activity_window")
	result, err := definition.Handler(context.Background(), CallContext{Scope: scope}, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("get_chat_activity_window handler error = %v", err)
	}
	assertToolDataSubset(t, result, map[string]any{
		"message_count":        4,
		"window_span_seconds":  int64(900),
		"average_gap_seconds":  300.0,
		"messages_in_last_10m": 3,
		"messages_in_last_1h":  4,
		"burstiness":           "steady",
	})
}

func TestGetChatActivityWindowBurstyAndScopeIsolated(t *testing.T) {
	store := memory.New(memory.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil))).Conversations()
	scope := state.ConversationScope{ChatID: 1, TopicID: 5}
	otherScope := state.ConversationScope{ChatID: 1, TopicID: 6}
	base := time.Date(2026, 4, 20, 10, 0, 0, 0, time.UTC)
	for i, ts := range []time.Time{
		base,
		base.Add(10 * time.Second),
		base.Add(20 * time.Second),
		base.Add(20 * time.Minute),
	} {
		store.AppendMessage(scope, state.Message{
			Name:      "alice",
			Text:      "bursty",
			MessageID: i + 1,
			CreatedAt: ts,
		})
	}
	store.AppendMessage(otherScope, state.Message{
		Name:      "mallory",
		Text:      "ignore me",
		MessageID: 99,
		CreatedAt: base.Add(25 * time.Minute),
	})

	runtime := New(
		&stubLLM{},
		store,
		&stubExtractor{},
		nil,
		&stubActionSender{},
		nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		Config{MaxIterations: 6},
	)

	definition, _ := runtime.registry.Lookup("get_chat_activity_window")
	result, err := definition.Handler(context.Background(), CallContext{Scope: scope}, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("get_chat_activity_window handler error = %v", err)
	}
	assertToolDataSubset(t, result, map[string]any{
		"message_count":        4,
		"window_span_seconds":  int64(1200),
		"messages_in_last_10m": 1,
		"messages_in_last_1h":  4,
		"burstiness":           "bursty",
	})

	data := result.Data.(map[string]any)
	if got := data["last_message_at"]; got != base.Add(20*time.Minute).Format(time.RFC3339) {
		t.Fatalf("unexpected last_message_at: %v", got)
	}
}

func TestGetHistoryBoundsEmpty(t *testing.T) {
	runtime := New(
		&stubLLM{},
		memory.New(memory.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil))).Conversations(),
		&stubExtractor{},
		nil,
		&stubActionSender{},
		nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		Config{MaxIterations: 6, RecentHistoryLimit: 2},
	)

	definition, ok := runtime.registry.Lookup("get_history_bounds")
	if !ok {
		t.Fatal("get_history_bounds not registered")
	}

	result, err := definition.Handler(context.Background(), CallContext{Scope: state.ConversationScope{ChatID: 1}}, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("get_history_bounds handler error = %v", err)
	}
	if result.Status != "empty" {
		t.Fatalf("expected empty status, got %+v", result)
	}
}

func TestGetHistoryBoundsReportsFullAndRecentHistory(t *testing.T) {
	store := memory.New(memory.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil))).Conversations()
	scope := state.ConversationScope{ChatID: 1, TopicID: 5}
	base := time.Date(2026, 4, 20, 10, 0, 0, 0, time.UTC)
	for i := 0; i < 4; i++ {
		store.AppendMessage(scope, state.Message{
			Name:      "alice",
			Text:      "message",
			MessageID: i + 1,
			CreatedAt: base.Add(time.Duration(i) * 5 * time.Minute),
		})
	}

	runtime := New(
		&stubLLM{},
		store,
		&stubExtractor{},
		nil,
		&stubActionSender{},
		nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		Config{MaxIterations: 6, RecentHistoryLimit: 2},
	)

	definition, _ := runtime.registry.Lookup("get_history_bounds")
	result, err := definition.Handler(context.Background(), CallContext{Scope: scope}, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("get_history_bounds handler error = %v", err)
	}
	if result.Status != "ok" {
		t.Fatalf("unexpected result: %+v", result)
	}

	data, ok := result.Data.(map[string]any)
	if !ok {
		t.Fatalf("unexpected data type: %T", result.Data)
	}
	fullHistory, ok := data["full_history"].(map[string]any)
	if !ok {
		t.Fatalf("unexpected full_history type: %T", data["full_history"])
	}
	recentHistory, ok := data["recent_history"].(map[string]any)
	if !ok {
		t.Fatalf("unexpected recent_history type: %T", data["recent_history"])
	}
	if got := fullHistory["message_count"]; got != 4 {
		t.Fatalf("unexpected full history count: %v", got)
	}
	if got := fullHistory["oldest_message_at"]; got != base.Format(time.RFC3339) {
		t.Fatalf("unexpected full oldest message time: %v", got)
	}
	if got := recentHistory["message_count"]; got != 2 {
		t.Fatalf("unexpected recent history count: %v", got)
	}
	if got := recentHistory["oldest_message_at"]; got != base.Add(10*time.Minute).Format(time.RFC3339) {
		t.Fatalf("unexpected recent oldest message time: %v", got)
	}
}

func TestGetMessageThreadContextBuildsChainAndAdjacentReplies(t *testing.T) {
	store := memory.New(memory.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil))).Conversations()
	scope := state.ConversationScope{ChatID: 1, TopicID: 7}
	base := time.Date(2026, 4, 20, 10, 0, 0, 0, time.UTC)

	root := state.Message{Name: "alice", Username: "alice", Text: "root", MessageID: 1, FromID: 10, CreatedAt: base}
	botReply := state.Message{
		Name:      "bot",
		Username:  "mybot",
		Text:      "bot reply",
		MessageID: 2,
		FromID:    99,
		IsMe:      true,
		ReplyTo:   &root,
		CreatedAt: base.Add(1 * time.Minute),
	}
	current := state.Message{
		Name:          "alice",
		Username:      "alice",
		Text:          "current request",
		MessageID:     3,
		FromID:        10,
		IsUserRequest: true,
		ReplyTo:       &botReply,
		CreatedAt:     base.Add(2 * time.Minute),
	}
	sideOnRoot := state.Message{
		Name:      "bob",
		Username:  "bob",
		Text:      "side root",
		MessageID: 4,
		FromID:    11,
		ReplyTo:   &root,
		CreatedAt: base.Add(30 * time.Second),
	}
	sideOnBot := state.Message{
		Name:      "carol",
		Username:  "carol",
		Text:      "side bot",
		MessageID: 5,
		FromID:    12,
		ReplyTo:   &botReply,
		CreatedAt: base.Add(90 * time.Second),
	}

	for _, msg := range []state.Message{root, botReply, current, sideOnRoot, sideOnBot} {
		store.AppendMessage(scope, msg)
	}

	runtime := New(
		&stubLLM{},
		store,
		&stubExtractor{},
		nil,
		&stubActionSender{},
		nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		Config{MaxIterations: 6},
	)

	definition, ok := runtime.registry.Lookup("get_message_thread_context")
	if !ok {
		t.Fatal("get_message_thread_context not registered")
	}

	result, err := definition.Handler(context.Background(), CallContext{Scope: scope, Requester: current}, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("get_message_thread_context handler error = %v", err)
	}
	if result.Status != "ok" {
		t.Fatalf("unexpected result: %+v", result)
	}

	data := result.Data.(map[string]any)
	if got := data["anchor_reason"]; got != "request_reply_target" {
		t.Fatalf("unexpected anchor_reason: %v", got)
	}
	if got := data["anchor_message_id"]; got != 2 {
		t.Fatalf("unexpected anchor_message_id: %v", got)
	}

	chain := data["chain"].([]map[string]any)
	if len(chain) != 3 {
		t.Fatalf("expected 3 chain messages, got %d", len(chain))
	}
	if got := []any{chain[0]["message_id"], chain[1]["message_id"], chain[2]["message_id"]}; !slices.Equal(got, []any{1, 2, 3}) {
		t.Fatalf("unexpected chain ids: %v", got)
	}

	adjacent := data["adjacent_replies"].([]map[string]any)
	if len(adjacent) != 2 {
		t.Fatalf("expected 2 adjacent groups, got %d", len(adjacent))
	}
	if got := adjacent[0]["parent_message_id"]; got != 1 {
		t.Fatalf("unexpected first adjacent parent: %v", got)
	}
	if replies := adjacent[0]["replies"].([]map[string]any); len(replies) != 1 || replies[0]["message_id"] != 4 {
		t.Fatalf("unexpected root adjacent replies: %#v", adjacent[0]["replies"])
	}
	if got := adjacent[1]["parent_message_id"]; got != 2 {
		t.Fatalf("unexpected second adjacent parent: %v", got)
	}
	if replies := adjacent[1]["replies"].([]map[string]any); len(replies) != 1 || replies[0]["message_id"] != 5 {
		t.Fatalf("unexpected bot adjacent replies: %#v", adjacent[1]["replies"])
	}
}

func TestGetMessageThreadContextFallsBackToEmbeddedReplyChain(t *testing.T) {
	store := memory.New(memory.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil))).Conversations()
	scope := state.ConversationScope{ChatID: 1}
	base := time.Date(2026, 4, 20, 12, 0, 0, 0, time.UTC)

	current := state.Message{
		Name:          "alice",
		Text:          "latest",
		MessageID:     33,
		FromID:        10,
		IsUserRequest: true,
		CreatedAt:     base,
		ReplyTo: &state.Message{
			Name:      "bot",
			Text:      "missing parent in flat snapshot",
			MessageID: 22,
			IsMe:      true,
			CreatedAt: base.Add(-1 * time.Minute),
			ReplyTo: &state.Message{
				Name:      "bob",
				Text:      "older root",
				MessageID: 11,
				FromID:    20,
				CreatedAt: base.Add(-2 * time.Minute),
			},
		},
	}

	store.AppendMessage(scope, state.Message{
		Name:          current.Name,
		Text:          current.Text,
		MessageID:     current.MessageID,
		FromID:        current.FromID,
		IsUserRequest: true,
		CreatedAt:     current.CreatedAt,
	})

	runtime := New(
		&stubLLM{},
		store,
		&stubExtractor{},
		nil,
		&stubActionSender{},
		nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		Config{MaxIterations: 6},
	)

	definition, _ := runtime.registry.Lookup("get_message_thread_context")
	result, err := definition.Handler(context.Background(), CallContext{Scope: scope, Requester: current}, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("get_message_thread_context handler error = %v", err)
	}

	data := result.Data.(map[string]any)
	chain := data["chain"].([]map[string]any)
	if got := []any{chain[0]["message_id"], chain[1]["message_id"], chain[2]["message_id"]}; !slices.Equal(got, []any{11, 22, 33}) {
		t.Fatalf("unexpected chain ids: %v", got)
	}
}

func TestGetMessageThreadContextFallsBackToCurrentMessageWhenNotReply(t *testing.T) {
	store := memory.New(memory.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil))).Conversations()
	scope := state.ConversationScope{ChatID: 1}
	current := state.Message{
		Name:          "alice",
		Text:          "standalone",
		MessageID:     7,
		FromID:        10,
		IsUserRequest: true,
		CreatedAt:     time.Date(2026, 4, 20, 13, 0, 0, 0, time.UTC),
	}
	store.AppendMessage(scope, current)

	runtime := New(
		&stubLLM{},
		store,
		&stubExtractor{},
		nil,
		&stubActionSender{},
		nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		Config{MaxIterations: 6},
	)

	definition, _ := runtime.registry.Lookup("get_message_thread_context")
	result, err := definition.Handler(context.Background(), CallContext{Scope: scope, Requester: current}, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("get_message_thread_context handler error = %v", err)
	}

	data := result.Data.(map[string]any)
	if got := data["anchor_reason"]; got != "request_message" {
		t.Fatalf("unexpected anchor_reason: %v", got)
	}
	if got := data["anchor_message_id"]; got != 7 {
		t.Fatalf("unexpected anchor_message_id: %v", got)
	}
}

func TestGetMessageThreadContextRespectsCapsAndTopicScope(t *testing.T) {
	store := memory.New(memory.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil))).Conversations()
	scope := state.ConversationScope{ChatID: 1, TopicID: 1}
	otherScope := state.ConversationScope{ChatID: 1, TopicID: 2}
	base := time.Date(2026, 4, 20, 14, 0, 0, 0, time.UTC)

	var current state.Message
	var parent *state.Message
	for i := 1; i <= 9; i++ {
		msg := state.Message{
			Name:      "alice",
			Text:      "chain",
			MessageID: i,
			FromID:    10,
			CreatedAt: base.Add(time.Duration(i) * time.Minute),
		}
		if parent != nil {
			replyCopy := *parent
			msg.ReplyTo = &replyCopy
		}
		store.AppendMessage(scope, msg)
		parent = &msg
		current = msg
	}

	for i := 0; i < 4; i++ {
		store.AppendMessage(scope, state.Message{
			Name:      "side",
			Text:      "adjacent",
			MessageID: 100 + i,
			FromID:    int64(20 + i),
			ReplyTo:   &state.Message{MessageID: 2},
			CreatedAt: base.Add(30*time.Second + time.Duration(i)*time.Second),
		})
	}
	for i := 0; i < 10; i++ {
		store.AppendMessage(otherScope, state.Message{
			Name:      "mallory",
			Text:      "ignore",
			MessageID: 200 + i,
			FromID:    99,
			ReplyTo:   &state.Message{MessageID: 1},
			CreatedAt: base.Add(time.Duration(i) * time.Second),
		})
	}

	runtime := New(
		&stubLLM{},
		store,
		&stubExtractor{},
		nil,
		&stubActionSender{},
		nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		Config{MaxIterations: 6},
	)

	definition, _ := runtime.registry.Lookup("get_message_thread_context")
	result, err := definition.Handler(context.Background(), CallContext{Scope: scope, Requester: current}, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("get_message_thread_context handler error = %v", err)
	}

	data := result.Data.(map[string]any)
	chain := data["chain"].([]map[string]any)
	if len(chain) != messageThreadChainLimit {
		t.Fatalf("expected chain cap %d, got %d", messageThreadChainLimit, len(chain))
	}
	if chain[0]["message_id"] != 2 || chain[len(chain)-1]["message_id"] != 9 {
		t.Fatalf("unexpected capped chain range: %#v", chain)
	}

	truncated := data["truncated"].(map[string]any)
	if got := truncated["chain_depth"]; got != true {
		t.Fatalf("expected chain truncation, got %v", got)
	}
	if got := truncated["adjacent_replies"]; got != true {
		t.Fatalf("expected adjacent truncation, got %v", got)
	}

	adjacent := data["adjacent_replies"].([]map[string]any)
	if len(adjacent) != 1 {
		t.Fatalf("expected one adjacent group in current topic, got %d", len(adjacent))
	}
	replies := adjacent[0]["replies"].([]map[string]any)
	if len(replies) != messageThreadAdjacentPerNodeMax {
		t.Fatalf("expected per-node adjacent cap %d, got %d", messageThreadAdjacentPerNodeMax, len(replies))
	}
}

func TestCurrentTimeHandlerIncludesServerTimezone(t *testing.T) {
	result, err := currentTimeHandler(context.Background(), CallContext{}, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("currentTimeHandler() error = %v", err)
	}
	if result.Status != "ok" {
		t.Fatalf("unexpected result: %+v", result)
	}

	data, ok := result.Data.(map[string]any)
	if !ok {
		t.Fatalf("unexpected data type: %T", result.Data)
	}
	if got, ok := data["current_time_rfc3339"].(string); !ok || got == "" {
		t.Fatalf("missing current_time_rfc3339: %#v", data["current_time_rfc3339"])
	}
	if got, ok := data["current_time_utc"].(string); !ok || got == "" {
		t.Fatalf("missing current_time_utc: %#v", data["current_time_utc"])
	}
	if got, ok := data["server_timezone"].(string); !ok || got == "" {
		t.Fatalf("missing server_timezone: %#v", data["server_timezone"])
	}
	if _, ok := data["utc_offset_seconds"].(int); !ok {
		t.Fatalf("unexpected utc_offset_seconds type: %T", data["utc_offset_seconds"])
	}
}

func TestSearchWebToolIsConditionalAndReturnsProvider(t *testing.T) {
	runtimeWithoutSearch := New(
		&stubLLM{},
		memory.New(memory.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil))).Conversations(),
		&stubExtractor{},
		nil,
		&stubActionSender{},
		nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		Config{MaxIterations: 6},
	)
	if _, ok := runtimeWithoutSearch.registry.Lookup("search_web"); ok {
		t.Fatal("did not expect search_web when search is unavailable")
	}

	runtimeWithSearch := New(
		&stubLLM{},
		memory.New(memory.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil))).Conversations(),
		&stubExtractor{},
		&stubSearcher{result: search.Result{
			Provider: "kagi",
			Query:    "golang",
			Results:  []search.ResultItem{{Title: "Go", URL: "https://go.dev"}},
		}},
		&stubActionSender{},
		nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		Config{MaxIterations: 6},
	)
	definition, ok := runtimeWithSearch.registry.Lookup("search_web")
	if !ok {
		t.Fatal("expected search_web when search is configured")
	}
	if definition.InvocationPolicy != InvocationPolicyDiscretionaryPaid {
		t.Fatalf("unexpected invocation policy: %q", definition.InvocationPolicy)
	}

	result, err := definition.Handler(context.Background(), CallContext{}, json.RawMessage(`{"query":"golang","max_results":3}`))
	if err != nil {
		t.Fatalf("search_web handler error = %v", err)
	}
	if result.Status != "ok" {
		t.Fatalf("unexpected result: %+v", result)
	}

	data, ok := result.Data.(search.Result)
	if !ok {
		t.Fatalf("unexpected data type: %T", result.Data)
	}
	if data.Provider != "kagi" {
		t.Fatalf("unexpected provider: %q", data.Provider)
	}
}

func TestDatetimeMathHandler(t *testing.T) {
	tests := []struct {
		name         string
		args         string
		wantStatus   string
		wantData     map[string]any
		wantError    string
		wantErrorMsg string
	}{
		{
			name:       "diff positive",
			args:       `{"operation":"diff","left":"2026-04-20T10:00:00+03:00","right":"2026-04-22T15:30:00+03:00"}`,
			wantStatus: "ok",
			wantData: map[string]any{
				"operation":        "diff",
				"duration_seconds": int64(192600),
				"duration_minutes": 3210.0,
				"duration_hours":   53.5,
				"duration_days":    2.2291666666666665,
				"sign":             1,
			},
		},
		{
			name:       "diff negative",
			args:       `{"operation":"diff","left":"2026-04-22T15:30:00+03:00","right":"2026-04-20T10:00:00+03:00"}`,
			wantStatus: "ok",
			wantData: map[string]any{
				"operation":        "diff",
				"duration_seconds": int64(-192600),
				"duration_minutes": -3210.0,
				"duration_hours":   -53.5,
				"duration_days":    -2.2291666666666665,
				"sign":             -1,
			},
		},
		{
			name:       "shift mixed units",
			args:       `{"operation":"shift","timestamp":"2026-04-20T12:00:00+03:00","days":1,"hours":-2,"minutes":30,"seconds":15}`,
			wantStatus: "ok",
			wantData: map[string]any{
				"operation": "shift",
				"input":     "2026-04-20T12:00:00+03:00",
				"result":    "2026-04-21T10:30:15+03:00",
			},
		},
		{
			name:       "shift zero delta stays stable",
			args:       `{"operation":"shift","timestamp":"2026-04-20T12:00:00+03:00","days":0}`,
			wantStatus: "ok",
			wantData: map[string]any{
				"operation": "shift",
				"input":     "2026-04-20T12:00:00+03:00",
				"result":    "2026-04-20T12:00:00+03:00",
			},
		},
		{
			name:       "weekday",
			args:       `{"operation":"weekday","timestamp":"2026-04-20T12:00:00+03:00"}`,
			wantStatus: "ok",
			wantData: map[string]any{
				"operation":     "weekday",
				"timestamp":     "2026-04-20T12:00:00+03:00",
				"weekday":       "Monday",
				"weekday_index": 1,
			},
		},
		{
			name:       "convert timezone across dst",
			args:       `{"operation":"convert_timezone","timestamp":"2026-03-29T01:30:00+00:00","target_timezone":"Europe/Oslo"}`,
			wantStatus: "ok",
			wantData: map[string]any{
				"operation":       "convert_timezone",
				"input":           "2026-03-29T01:30:00Z",
				"target_timezone": "Europe/Oslo",
				"result":          "2026-03-29T03:30:00+02:00",
			},
		},
		{
			name:         "missing shift fields",
			args:         `{"operation":"shift","timestamp":"2026-04-20T12:00:00+03:00"}`,
			wantStatus:   "error",
			wantError:    "empty_shift",
			wantErrorMsg: "shift requires at least one shift field",
		},
		{
			name:         "invalid timestamp",
			args:         `{"operation":"weekday","timestamp":"nope"}`,
			wantStatus:   "error",
			wantError:    "invalid_timestamp",
			wantErrorMsg: "timestamp must be a valid RFC3339 timestamp",
		},
		{
			name:         "invalid timezone",
			args:         `{"operation":"convert_timezone","timestamp":"2026-04-20T12:00:00+03:00","target_timezone":"Nope/Nowhere"}`,
			wantStatus:   "error",
			wantError:    "invalid_timezone",
			wantErrorMsg: "target_timezone must be a valid IANA timezone",
		},
		{
			name:         "invalid operation",
			args:         `{"operation":"warp","timestamp":"2026-04-20T12:00:00+03:00"}`,
			wantStatus:   "error",
			wantError:    "invalid_operation",
			wantErrorMsg: "operation must be one of diff, shift, weekday, convert_timezone",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result, err := datetimeMathHandler(context.Background(), CallContext{}, json.RawMessage(tc.args))
			if err != nil {
				t.Fatalf("datetimeMathHandler() error = %v", err)
			}
			if result.Status != tc.wantStatus {
				t.Fatalf("unexpected result: %+v", result)
			}

			if tc.wantStatus == "error" {
				assertToolError(t, result, tc.wantError, tc.wantErrorMsg)

				return
			}

			assertToolDataSubset(t, result, tc.wantData)
		})
	}
}

func TestDatetimeFormatHandler(t *testing.T) {
	tests := []struct {
		name         string
		args         string
		wantStatus   string
		wantData     map[string]any
		wantError    string
		wantErrorMsg string
	}{
		{
			name:       "short",
			args:       `{"timestamp":"2026-04-20T10:00:00+03:00","style":"short"}`,
			wantStatus: "ok",
			wantData: map[string]any{
				"formatted":  "2026-04-20 10:00",
				"style":      "short",
				"utc_offset": "+03:00",
			},
		},
		{
			name:       "long with timezone conversion",
			args:       `{"timestamp":"2026-03-29T01:30:00+00:00","style":"long","target_timezone":"Europe/Oslo"}`,
			wantStatus: "ok",
			wantData: map[string]any{
				"formatted":       "2026-03-29 03:30 CEST",
				"style":           "long",
				"target_timezone": "Europe/Oslo",
				"timezone":        "CEST",
				"utc_offset":      "+02:00",
			},
		},
		{
			name:       "date only",
			args:       `{"timestamp":"2026-04-20T10:00:00+03:00","style":"date_only"}`,
			wantStatus: "ok",
			wantData: map[string]any{
				"formatted": "2026-04-20",
			},
		},
		{
			name:       "time only",
			args:       `{"timestamp":"2026-04-20T10:00:00+03:00","style":"time_only"}`,
			wantStatus: "ok",
			wantData: map[string]any{
				"formatted": "10:00",
			},
		},
		{
			name:       "weekday date",
			args:       `{"timestamp":"2026-04-20T10:00:00+03:00","style":"weekday_date"}`,
			wantStatus: "ok",
			wantData: map[string]any{
				"formatted": "Monday, 2026-04-20",
			},
		},
		{
			name:         "invalid style",
			args:         `{"timestamp":"2026-04-20T10:00:00+03:00","style":"fancy"}`,
			wantStatus:   "error",
			wantError:    "invalid_style",
			wantErrorMsg: "style must be one of short, long, date_only, time_only, weekday_date",
		},
		{
			name:         "invalid timestamp",
			args:         `{"timestamp":"bad","style":"short"}`,
			wantStatus:   "error",
			wantError:    "invalid_timestamp",
			wantErrorMsg: "timestamp must be a valid RFC3339 timestamp",
		},
		{
			name:         "invalid timezone",
			args:         `{"timestamp":"2026-04-20T10:00:00+03:00","style":"short","target_timezone":"Mars/Base"}`,
			wantStatus:   "error",
			wantError:    "invalid_timezone",
			wantErrorMsg: "target_timezone must be a valid IANA timezone",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result, err := datetimeFormatHandler(context.Background(), CallContext{}, json.RawMessage(tc.args))
			if err != nil {
				t.Fatalf("datetimeFormatHandler() error = %v", err)
			}
			if result.Status != tc.wantStatus {
				t.Fatalf("unexpected result: %+v", result)
			}

			if tc.wantStatus == "error" {
				assertToolError(t, result, tc.wantError, tc.wantErrorMsg)

				return
			}

			assertToolDataSubset(t, result, tc.wantData)
		})
	}
}

func assertToolDataSubset(t *testing.T, result toolResult, want map[string]any) {
	t.Helper()

	data, ok := result.Data.(map[string]any)
	if !ok {
		t.Fatalf("unexpected data type: %T", result.Data)
	}

	for key, expected := range want {
		if got := data[key]; got != expected {
			t.Fatalf("unexpected %s: got %v want %v", key, got, expected)
		}
	}
}

func assertToolError(t *testing.T, result toolResult, wantCode, wantMessage string) {
	t.Helper()

	data, ok := result.Data.(map[string]any)
	if !ok {
		t.Fatalf("unexpected data type: %T", result.Data)
	}

	errPayload, ok := data["error"].(toolErrorPayload)
	if !ok {
		t.Fatalf("unexpected error payload type: %T", data["error"])
	}
	if errPayload.Code != wantCode || errPayload.Message != wantMessage {
		t.Fatalf("unexpected error payload: %+v", errPayload)
	}
}
