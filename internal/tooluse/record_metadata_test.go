package tooluse

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sort"
	"strings"
	"testing"

	"telegram-ollama-reply-bot/internal/execution"
	"telegram-ollama-reply-bot/internal/llm"
)

func okHandler(context.Context, CallContext, json.RawMessage) (toolResult, error) {
	return toolResult{Status: execution.StatusOK}, nil
}

func toolCalls(names ...string) llm.Response {
	calls := make([]llm.ToolCall, 0, len(names))
	for i, name := range names {
		calls = append(calls, llm.ToolCall{ID: "c" + string(rune('a'+i)), Name: name, Arguments: json.RawMessage(`{}`)})
	}

	return llm.Response{Message: llm.Message{Role: llm.RoleAssistant, ToolCalls: calls}}
}

func TestAdvertisedToolsAreTheUnionAcrossIterationsCountedOncePerRun(t *testing.T) {
	r, model := restrictedRuntime(t, &stubActionSender{})
	betaAvailable := true
	r.registry.Register(Definition{Name: "alpha", Handler: func(context.Context, CallContext, json.RawMessage) (toolResult, error) {
		betaAvailable = false // dynamic availability changes mid-run

		return toolResult{Status: execution.StatusOK}, nil
	}})
	r.registry.Register(Definition{Name: "beta", Handler: okHandler,
		Resolve: func(_ CallContext, p Presentation) (Presentation, bool) { return p, betaAvailable }})
	model.responses = []llm.Response{toolCalls("alpha"), {Message: llm.TextMessage(llm.RoleAssistant, "done")}}

	result, err := r.ReplyWithTools(context.Background(), ChatRequest{AllowedTools: []string{"alpha", "beta"}})
	if err != nil {
		t.Fatal(err)
	}
	if model.buildCount != 2 {
		t.Fatalf("fixture needs two generation iterations, got %d", model.buildCount)
	}
	// beta was advertised in the first request only; both are counted once.
	if want := []string{"alpha", "beta"}; !slices.Equal(result.AdvertisedTools, want) {
		t.Fatalf("AdvertisedTools = %v, want %v", result.AdvertisedTools, want)
	}
	if !sort.StringsAreSorted(result.AdvertisedTools) {
		t.Fatalf("AdvertisedTools must be sorted: %v", result.AdvertisedTools)
	}
	for _, d := range model.lastBuildTools {
		if d.Name == "beta" {
			t.Fatal("beta must not be advertised in the second iteration")
		}
	}
}

func TestIterationWhoseRequestPreparationFailedAdvertisesNothing(t *testing.T) {
	r, model := restrictedRuntime(t, &stubActionSender{})
	model.buildErr = errors.New("prepare failed")

	result, err := r.ReplyWithTools(context.Background(), ChatRequest{})
	if err == nil {
		t.Fatal("expected the preparation error")
	}
	if len(result.AdvertisedTools) != 0 {
		t.Fatalf("a request that was never submitted advertises nothing, got %v", result.AdvertisedTools)
	}
}

func TestAdvertisedToolsFollowTheEffectiveSystemOneModel(t *testing.T) {
	r, model := restrictedRuntime(t, &stubActionSender{})
	r.AttachDecisionTools(&stubDecisionClient{})

	model.responses = []llm.Response{{Message: llm.TextMessage(llm.RoleAssistant, "x")}}
	without, err := r.ReplyWithTools(context.Background(), ChatRequest{})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{decisionChooseToolName, decisionAssessToolName} {
		if slices.Contains(without.AdvertisedTools, name) {
			t.Fatalf("%s must be excluded when no effective System One model is configured", name)
		}
	}

	model.responses = []llm.Response{{Message: llm.TextMessage(llm.RoleAssistant, "x")}}
	with, err := r.ReplyWithTools(context.Background(), ChatRequest{TurnConfig: llm.TurnConfig{SystemOneModel: "decider"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{decisionChooseToolName, decisionAssessToolName} {
		if !slices.Contains(with.AdvertisedTools, name) {
			t.Fatalf("%s must be advertised with a System One model", name)
		}
	}
	// They are registered either way: the catalog distinguishes "never
	// advertised" from "unregistered".
	if !slices.Contains(r.ToolNames(), decisionChooseToolName) {
		t.Fatal("decision tools stay in the registered catalog")
	}
}

func TestRejectionReasonsAreDistinguishedOnTheRecord(t *testing.T) {
	r, model := restrictedRuntime(t, &stubActionSender{})
	available := true
	r.registry.Register(Definition{Name: "flip", Handler: func(context.Context, CallContext, json.RawMessage) (toolResult, error) {
		available = false

		return toolResult{Status: execution.StatusOK}, nil
	}})
	r.registry.Register(Definition{Name: "dyn", Handler: okHandler,
		Resolve: func(_ CallContext, p Presentation) (Presentation, bool) { return p, available }})
	r.registry.Register(Definition{Name: "hidden", Handler: okHandler})
	model.responses = []llm.Response{
		toolCalls("no_such_tool", "hidden", "flip", "dyn"),
		{Message: llm.TextMessage(llm.RoleAssistant, "done")},
	}

	result, err := r.ReplyWithTools(context.Background(), ChatRequest{AllowedTools: []string{"flip", "dyn"}})
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		name      string
		rejection Rejection
		executed  bool
	}{
		{"no_such_tool", RejectionNotRegistered, false},
		{"hidden", RejectionNotAdvertised, false},
		{"flip", RejectionNone, true},
		{"dyn", RejectionUnavailable, false},
	}
	if len(result.ToolCalls) != len(want) {
		t.Fatalf("records = %+v", result.ToolCalls)
	}
	for i, w := range want {
		got := result.ToolCalls[i]
		if got.Name != w.name || got.Rejection != w.rejection || got.Executed != w.executed {
			t.Fatalf("record %d = %+v, want %+v", i, got, w)
		}
		if !w.executed && (got.ErrorCode != "tool_unavailable" || got.ErrorClass != ErrorUnavailable || got.Duration != 0) {
			t.Fatalf("every rejection keeps the shared model-facing error: %+v", got)
		}
	}
}

func TestEmptyStatusIsAnExecutedNonErrorRecord(t *testing.T) {
	r, model := restrictedRuntime(t, &stubActionSender{})
	r.registry.Register(Definition{Name: "search", Handler: func(context.Context, CallContext, json.RawMessage) (toolResult, error) {
		return toolResult{Status: execution.StatusEmpty}, nil
	}})
	model.responses = []llm.Response{toolCalls("search"), {Message: llm.TextMessage(llm.RoleAssistant, "nothing found")}}

	result, err := r.ReplyWithTools(context.Background(), ChatRequest{AllowedTools: []string{"search"}})
	if err != nil {
		t.Fatal(err)
	}
	rec := result.ToolCalls[0]
	if !rec.Executed || rec.Status != execution.StatusEmpty || rec.ErrorCode != "" || rec.Rejection != RejectionNone {
		t.Fatalf("a valid empty result is an executed success: %+v", rec)
	}
}

func TestResultPreparationClassification(t *testing.T) {
	big := make([]any, 0, 200)
	for i := 0; i < 200; i++ {
		big = append(big, map[string]any{"message_id": i, "text": strings.Repeat("x", 200)})
	}
	cases := []struct {
		name   string
		result toolResult
		budget int
		want   ResultPreparation
	}{
		{"within budget", toolResult{Status: execution.StatusOK, Data: map[string]any{"a": 1}}, 4000, ResultPreparationComplete},
		{"reduced to fit", toolResult{Status: execution.StatusOK, Data: map[string]any{"items": big}}, 2000, ResultPreparationReduced},
		{"unserializable", toolResult{Status: execution.StatusOK, Data: make(chan int)}, 4000, ResultPreparationFailed},
		{"tool-set truncation is not a reduction", toolResult{Status: execution.StatusOK, Truncated: true, Data: map[string]any{"a": 1}}, 4000, ResultPreparationComplete},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			text, got := prepareResult(tc.result, tc.budget)
			if got != tc.want {
				t.Fatalf("preparation = %q, want %q (%s)", got, tc.want, text)
			}
		})
	}
}

func TestResultPreparationFailureKeepsConfirmedSideEffectAndExecutionStatus(t *testing.T) {
	r, model := restrictedRuntime(t, &stubActionSender{})
	r.registry.Register(Definition{Name: "effect", SideEffecting: true, Handler: func(context.Context, CallContext, json.RawMessage) (toolResult, error) {
		return toolResult{Status: execution.StatusOK, Data: make(chan int)}, nil
	}})
	model.responses = []llm.Response{toolCalls("effect"), {Message: llm.TextMessage(llm.RoleAssistant, "done")}}

	result, err := r.ReplyWithTools(context.Background(), ChatRequest{AllowedTools: []string{"effect"}})
	if err != nil {
		t.Fatal(err)
	}
	rec := result.ToolCalls[0]
	if rec.ResultPreparation != ResultPreparationFailed {
		t.Fatalf("preparation = %q", rec.ResultPreparation)
	}
	if rec.Status != execution.StatusOK || rec.SideEffectOutcome != "confirmed" || !rec.Executed || rec.ErrorCode != "" {
		t.Fatalf("handler outcome must stay authoritative and independent of preparation: %+v", rec)
	}
}

func TestRecordsAndAdvertisementSurviveLoopExhaustion(t *testing.T) {
	r, model := restrictedRuntime(t, &stubActionSender{})
	r.config.MaxIterations = 2
	r.registry.Register(Definition{Name: "probe", Handler: okHandler})
	model.responses = []llm.Response{toolCalls("probe"), toolCalls("probe"), toolCalls("probe")}

	result, err := r.ReplyWithTools(context.Background(), ChatRequest{AllowedTools: []string{"probe"}})
	if !errors.Is(err, ErrToolLoopLimitReached) {
		t.Fatalf("expected the loop limit, got %v", err)
	}
	if len(result.ToolCalls) != 2 || !slices.Equal(result.AdvertisedTools, []string{"probe"}) {
		t.Fatalf("a failed run must retain its records and advertisement: %+v", result)
	}
}

func TestToolNamesReturnsACopyOfTheCatalogInRegistrationOrder(t *testing.T) {
	r, _ := restrictedRuntime(t, &stubActionSender{})
	r.registry.Register(Definition{Name: "zz_last", Handler: okHandler})

	names := r.ToolNames()
	if names[len(names)-1] != "zz_last" || !slices.Contains(names, "say_nothing") {
		t.Fatalf("unexpected catalog %v", names)
	}
	names[0] = "mutated"
	if r.ToolNames()[0] == "mutated" {
		t.Fatal("ToolNames must return a copy")
	}
}
