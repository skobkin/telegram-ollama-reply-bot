package tooluse

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"

	"telegram-ollama-reply-bot/internal/content/search"
	"telegram-ollama-reply-bot/internal/llm"
	"telegram-ollama-reply-bot/internal/state/memory"
)

// countingSearcher records how many searches actually reached the backend,
// which is the paid resource the permission protects.
type countingSearcher struct {
	calls atomic.Int32
}

func (s *countingSearcher) Search(context.Context, search.Request) (search.Result, error) {
	s.calls.Add(1)

	return search.Result{Provider: "stub", Query: "q", Results: []search.ResultItem{{Title: "Go", URL: "https://go.dev"}}}, nil
}

func newSearchGateRuntime(stub *stubLLM, searcher search.Searcher) *Runtime {
	return New(
		stub,
		memory.New(memory.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil))).Conversations(),
		&stubExtractor{},
		searcher,
		&stubActionSender{},
		nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		Config{MaxIterations: 6},
	)
}

func searchGateRequest(enabled bool) ChatRequest {
	req := eventRequest()
	req.TurnConfig = llm.TurnConfig{WebSearchEnabled: enabled}

	return req
}

func toolDefinitionNames(tools []llm.ToolDefinition) []string {
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		names = append(names, tool.Name)
	}

	return names
}

// The permission hides search_web from the model everywhere it is advertised:
// the tool schemas, the policy text, and the recorded advertised set.
func TestSearchWebAdvertisedOnlyWhenPermitted(t *testing.T) {
	tests := []struct {
		name    string
		enabled bool
	}{
		{"enabled", true},
		{"disabled", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stub := &stubLLM{responses: []llm.Response{{Message: llm.TextMessage(llm.RoleAssistant, "done")}}}
			runtime := newSearchGateRuntime(stub, &countingSearcher{})

			result, err := runtime.ReplyWithTools(context.Background(), searchGateRequest(tt.enabled))
			if err != nil {
				t.Fatal(err)
			}

			inSchemas := false
			for _, name := range toolDefinitionNames(stub.lastBuildTools) {
				if name == "search_web" {
					inSchemas = true
				}
			}
			inAdvertised := false
			for _, name := range result.AdvertisedTools {
				if name == "search_web" {
					inAdvertised = true
				}
			}
			inPolicy := strings.Contains(stub.lastBuildPolicy, "search_web")

			for label, got := range map[string]bool{"tool schemas": inSchemas, "advertised set": inAdvertised, "tool policy": inPolicy} {
				if got != tt.enabled {
					t.Errorf("search_web present in %s = %v, want %v", label, got, tt.enabled)
				}
			}
		})
	}
}

// A model that names search_web anyway (stale instructions, prompt injection)
// must be rejected by the runtime without reaching the search backend.
func TestSearchWebCallRejectedWhenDisabled(t *testing.T) {
	stub := &stubLLM{responses: []llm.Response{
		{Message: llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{
			{ID: "call-1", Name: "search_web", Arguments: json.RawMessage(`{"query":"golang"}`)},
		}}},
		{Message: llm.TextMessage(llm.RoleAssistant, "done")},
	}}
	searcher := &countingSearcher{}
	runtime := newSearchGateRuntime(stub, searcher)

	result, err := runtime.ReplyWithTools(context.Background(), searchGateRequest(false))
	if err != nil {
		t.Fatal(err)
	}

	if got := searcher.calls.Load(); got != 0 {
		t.Fatalf("search backend was called %d times for a disabled chat", got)
	}
	if len(result.ToolCalls) != 1 {
		t.Fatalf("records = %+v", result.ToolCalls)
	}
	record := result.ToolCalls[0]
	if record.Name != "search_web" || record.Executed || record.Rejection != RejectionNotAdvertised {
		t.Fatalf("record = %+v", record)
	}
	if len(stub.lastBuildExtras) != 2 || !strings.Contains(stub.lastBuildExtras[1].Text(), "tool_unavailable") {
		t.Fatalf("model must receive tool_unavailable, got %+v", stub.lastBuildExtras)
	}
}

func TestSearchWebCallExecutesWhenEnabled(t *testing.T) {
	stub := &stubLLM{responses: []llm.Response{
		{Message: llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{
			{ID: "call-1", Name: "search_web", Arguments: json.RawMessage(`{"query":"golang"}`)},
		}}},
		{Message: llm.TextMessage(llm.RoleAssistant, "done")},
	}}
	searcher := &countingSearcher{}
	runtime := newSearchGateRuntime(stub, searcher)

	result, err := runtime.ReplyWithTools(context.Background(), searchGateRequest(true))
	if err != nil {
		t.Fatal(err)
	}

	if got := searcher.calls.Load(); got != 1 {
		t.Fatalf("search backend calls = %d, want 1", got)
	}
	if len(result.ToolCalls) != 1 || !result.ToolCalls[0].Executed || result.ToolCalls[0].Rejection != RejectionNone {
		t.Fatalf("records = %+v", result.ToolCalls)
	}
}

// Without a configured backend the tool is never registered, so the
// permission cannot create a search capability.
func TestSearchWebPermissionCannotCreateCapability(t *testing.T) {
	stub := &stubLLM{responses: []llm.Response{{Message: llm.TextMessage(llm.RoleAssistant, "done")}}}
	runtime := newSearchGateRuntime(stub, nil)

	result, err := runtime.ReplyWithTools(context.Background(), searchGateRequest(true))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range result.AdvertisedTools {
		if name == "search_web" {
			t.Fatal("search_web must not be advertised without a search backend")
		}
	}
}

// The gate fails closed: a run without the permission (nil run state or an
// unset flag) never sees the tool.
func TestWebSearchPresentationFailsClosed(t *testing.T) {
	t.Parallel()

	presentation := Presentation{Description: "d", Parameters: json.RawMessage(`{}`)}

	if _, ok := webSearchPresentation(CallContext{}, presentation); ok {
		t.Error("nil run state must not expose search_web")
	}
	if _, ok := webSearchPresentation(CallContext{run: &runState{}}, presentation); ok {
		t.Error("unset permission must not expose search_web")
	}
	got, ok := webSearchPresentation(CallContext{run: &runState{webSearchEnabled: true}}, presentation)
	if !ok || got.Description != "d" || string(got.Parameters) != `{}` {
		t.Errorf("enabled run must expose search_web unchanged, got %+v ok=%v", got, ok)
	}
}
