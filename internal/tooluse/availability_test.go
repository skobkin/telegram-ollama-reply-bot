package tooluse

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"testing"

	"telegram-ollama-reply-bot/internal/llm"
	"telegram-ollama-reply-bot/internal/state"
)

func TestDynamicAvailabilityRecheckedWithinBatchAndIteration(t *testing.T) {
	r, model := restrictedRuntime(t, &stubActionSender{})
	available := true
	calls := 0
	schema := json.RawMessage(`{"type":"object"}`)
	r.registry.Register(Definition{Name: "dynamic", Description: "Native description only", Parameters: schema,
		Resolve: func(_ CallContext, p Presentation) (Presentation, bool) {
			// Even an in-place edit must never reach shared registry storage.
			p.Parameters[0] = ' '
			p.Parameters = json.RawMessage(`{"type":"object","properties":{"ready":{"type":"boolean"}}}`)

			return p, available
		},
		Handler: func(context.Context, CallContext, json.RawMessage) (toolResult, error) {
			calls++
			available = false

			return toolResult{Status: "ok"}, nil
		},
	})
	model.responses = []llm.Response{
		{Message: llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "one", Name: "dynamic"}, {ID: "two", Name: "dynamic"}}}},
		{Message: llm.TextMessage(llm.RoleAssistant, "done")},
	}
	result, err := r.ReplyWithTools(context.Background(), ChatRequest{})
	if err != nil || calls != 1 || len(result.ToolCalls) != 2 || result.ToolCalls[1].Status != "error" {
		t.Fatalf("result=%+v err=%v calls=%d", result, err, calls)
	}
	for _, d := range model.lastBuildTools {
		if d.Name == "dynamic" {
			t.Fatal("unavailable tool advertised on next iteration")
		}
	}
	if strings.Contains(model.lastBuildPolicy, "dynamic") {
		t.Fatal("unavailable tool in policy")
	}
	if string(schema) != `{"type":"object"}` {
		t.Fatal("registry schema mutated")
	}
}

func TestPolicyUsesNamesAndAdvisoryCategories(t *testing.T) {
	policy := buildToolPolicy([]Definition{{Name: "probe", Description: "do not duplicate", InvocationPolicy: InvocationPolicyDiscretionaryPaid}})
	if strings.Contains(policy, "do not duplicate") || !strings.Contains(policy, "- probe") || !strings.Contains(policy, "Discretionary paid tools") || !strings.Contains(policy, "retry with corrected arguments") {
		t.Fatal(policy)
	}
}

func TestAvailabilityRevokedDuringGenerationRejectsDispatch(t *testing.T) {
	r, m := restrictedRuntime(t, &stubActionSender{})
	available := true
	calls := 0
	r.registry.Register(Definition{Name: "revocable", Resolve: func(_ CallContext, p Presentation) (Presentation, bool) { return p, available }, Handler: func(context.Context, CallContext, json.RawMessage) (toolResult, error) {
		calls++

		return toolResult{Status: "ok"}, nil
	}})
	m.responses = []llm.Response{toolBatch(llm.ToolCall{ID: "call", Name: "revocable"}), {Message: llm.TextMessage(llm.RoleAssistant, "done")}}
	m.generateHook = func() { available = false }
	result, err := r.ReplyWithTools(context.Background(), ChatRequest{})
	if err != nil || calls != 0 || len(result.ToolCalls) != 1 || result.ToolCalls[0].Executed || result.ToolCalls[0].ErrorClass != ErrorUnavailable {
		t.Fatal(result, err, calls)
	}
}

func TestConcurrentPresentationsDoNotMutateRegistry(t *testing.T) {
	r := newRegistryForTest(t)
	raw := json.RawMessage(`{"type":"object"}`)
	r.registry.Register(Definition{Name: "scoped", Description: "base", Parameters: raw, Resolve: func(call CallContext, p Presentation) (Presentation, bool) {
		p.Parameters[0] = ' '
		p.Parameters = json.RawMessage(`{"type":"object","description":"` + strconv.FormatInt(call.Scope.ChatID, 10) + `"}`)
		p.Description = strconv.FormatInt(call.Scope.ChatID, 10)

		return p, true
	}})
	var wg sync.WaitGroup
	for id := int64(1); id <= 8; id++ {
		wg.Go(func() {
			for i := 0; i < 50; i++ {
				defs := r.resolveDefinitions(ChatRequest{Scope: state.ConversationScope{ChatID: id}, AllowedTools: []string{"scoped"}}, &runState{})
				if len(defs) != 1 || defs[0].Description != strconv.FormatInt(id, 10) || !json.Valid(defs[0].Parameters) {
					t.Errorf("cross-run definition leak: %+v", defs)
				}
			}
		})
	}
	wg.Wait()
	base, _ := r.registry.Lookup("scoped")
	if base.Description != "base" || string(raw) != `{"type":"object"}` {
		t.Fatal("registry changed")
	}
}
