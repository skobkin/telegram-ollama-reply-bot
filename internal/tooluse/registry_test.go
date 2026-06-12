package tooluse

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"slices"
	"testing"

	"telegram-ollama-reply-bot/internal/state/memory"
)

func newRegistryForTest(t *testing.T) *Runtime {
	t.Helper()

	return New(
		&stubLLM{},
		memory.New(memory.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil))).Conversations(),
		&stubExtractor{},
		nil,
		&stubActionSender{},
		nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		Config{MaxIterations: 6},
	)
}

func namesOf(defs []Definition) []string {
	out := make([]string, 0, len(defs))
	for _, d := range defs {
		out = append(out, d.Name)
	}

	return out
}

func TestRegistryRegisterAddsNewToolToDefaultSet(t *testing.T) {
	runtime := newRegistryForTest(t)

	def := Definition{
		Name:             "mcp_demo__echo",
		Description:      "echo input",
		Parameters:       json.RawMessage(`{"type":"object"}`),
		InvocationPolicy: InvocationPolicyDiscretionary,
		ResultCharBudget: 100,
		Handler: func(_ context.Context, _ CallContext, args json.RawMessage) (toolResult, error) {
			return toolResult{Status: "ok", Data: map[string]any{"echoed": string(args)}}, nil
		},
	}

	runtime.registry.Register(def)

	names := namesOf(runtime.registry.DefaultDefinitions())
	if !slices.Contains(names, "mcp_demo__echo") {
		t.Fatalf("expected mcp_demo__echo in default definitions, got %v", names)
	}

	got, ok := runtime.registry.Lookup("mcp_demo__echo")
	if !ok {
		t.Fatalf("expected lookup of mcp_demo__echo to succeed")
	}
	if got.ResultCharBudget != 100 {
		t.Fatalf("unexpected ResultCharBudget: %d", got.ResultCharBudget)
	}
}

func TestRegistryRegisterReplacesExistingDefinitionAndDedupesDefaults(t *testing.T) {
	runtime := newRegistryForTest(t)

	first := Definition{
		Name:             "mcp_demo__echo",
		Description:      "first",
		Parameters:       json.RawMessage(`{"type":"object"}`),
		InvocationPolicy: InvocationPolicyDiscretionary,
		ResultCharBudget: 100,
		Handler: func(_ context.Context, _ CallContext, _ json.RawMessage) (toolResult, error) {
			return toolResult{Status: "ok"}, nil
		},
	}
	second := Definition{
		Name:             "mcp_demo__echo",
		Description:      "second",
		Parameters:       json.RawMessage(`{"type":"object"}`),
		InvocationPolicy: InvocationPolicyExplicitRequestOnly,
		ResultCharBudget: 200,
		Handler: func(_ context.Context, _ CallContext, _ json.RawMessage) (toolResult, error) {
			return toolResult{Status: "ok", Summary: "second"}, nil
		},
	}

	runtime.registry.Register(first)
	runtime.registry.Register(second)

	defs := runtime.registry.DefaultDefinitions()
	count := 0
	for _, d := range defs {
		if d.Name == "mcp_demo__echo" {
			count++
			if d.Description != "second" {
				t.Fatalf("expected replacement description, got %q", d.Description)
			}
			if d.InvocationPolicy != InvocationPolicyExplicitRequestOnly {
				t.Fatalf("expected replacement invocation policy, got %q", d.InvocationPolicy)
			}
		}
	}
	if count != 1 {
		t.Fatalf("expected mcp_demo__echo to appear exactly once in defaults, got %d", count)
	}
}

func TestRegistryRegisterIsIdempotent(t *testing.T) {
	runtime := newRegistryForTest(t)

	def := Definition{
		Name:             "mcp_demo__echo",
		Description:      "echo",
		Parameters:       json.RawMessage(`{"type":"object"}`),
		InvocationPolicy: InvocationPolicyDiscretionary,
		ResultCharBudget: 100,
		Handler: func(_ context.Context, _ CallContext, _ json.RawMessage) (toolResult, error) {
			return toolResult{Status: "ok"}, nil
		},
	}

	runtime.registry.Register(def)
	runtime.registry.Register(def)
	runtime.registry.Register(def)

	count := 0
	for _, d := range runtime.registry.DefaultDefinitions() {
		if d.Name == "mcp_demo__echo" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("expected exactly one entry in defaults after repeated Register, got %d", count)
	}
}

func TestRegistryRegisterAllowsCollisionWithBuiltInBecauseNamingIsCallerResponsibility(t *testing.T) {
	// Built-in tool names are namespaced and unique within the tooluse package.
	// Register is intentionally permissive: it does not police collisions with
	// built-in tool names. The MCP translator (and any other caller) is
	// expected to namespace its names to avoid collisions. This test pins
	// that contract: callers MUST namespace; Register does not enforce it.
	runtime := newRegistryForTest(t)

	def := Definition{
		Name:             "mcp_demo__get_current_time",
		Description:      "namespaced clone of a built-in",
		Parameters:       json.RawMessage(`{"type":"object"}`),
		InvocationPolicy: InvocationPolicyDiscretionary,
		ResultCharBudget: 100,
		Handler: func(_ context.Context, _ CallContext, _ json.RawMessage) (toolResult, error) {
			return toolResult{Status: "ok"}, nil
		},
	}

	runtime.registry.Register(def)

	got, ok := runtime.registry.Lookup("mcp_demo__get_current_time")
	if !ok {
		t.Fatalf("expected namespaced clone to be registered")
	}
	if got.Description != "namespaced clone of a built-in" {
		t.Fatalf("expected the newly registered definition to win, got %q", got.Description)
	}
}

func TestRegistryDefaultDefinitionsPreservesRegistrationOrder(t *testing.T) {
	runtime := newRegistryForTest(t)

	for _, name := range []string{"mcp_demo__alpha", "mcp_demo__beta", "mcp_demo__gamma"} {
		runtime.registry.Register(Definition{
			Name:             name,
			Description:      name,
			Parameters:       json.RawMessage(`{"type":"object"}`),
			InvocationPolicy: InvocationPolicyDiscretionary,
			ResultCharBudget: 100,
			Handler: func(_ context.Context, _ CallContext, _ json.RawMessage) (toolResult, error) {
				return toolResult{Status: "ok"}, nil
			},
		})
	}

	defs := runtime.registry.DefaultDefinitions()
	want := []string{"mcp_demo__alpha", "mcp_demo__beta", "mcp_demo__gamma"}
	got := make([]string, 0, len(want))
	for _, d := range defs {
		if d.Name == want[0] || d.Name == want[1] || d.Name == want[2] {
			got = append(got, d.Name)
		}
	}
	if !slices.Equal(got, want) {
		t.Fatalf("expected registration order %v, got %v", want, got)
	}
}

func TestRegistryDefaultDefinitionsStillReturnsBuiltins(t *testing.T) {
	// Sanity check: after registering new tools, the built-in defaults must
	// remain present (i.e. Register does not clear or replace the default set).
	runtime := newRegistryForTest(t)

	runtime.registry.Register(Definition{
		Name:             "mcp_demo__echo",
		Description:      "echo",
		Parameters:       json.RawMessage(`{"type":"object"}`),
		InvocationPolicy: InvocationPolicyDiscretionary,
		ResultCharBudget: 100,
		Handler: func(_ context.Context, _ CallContext, _ json.RawMessage) (toolResult, error) {
			return toolResult{Status: "ok"}, nil
		},
	})

	names := namesOf(runtime.registry.DefaultDefinitions())
	for _, required := range []string{"get_current_time", "send_poll", "fetch_url_content"} {
		if !slices.Contains(names, required) {
			t.Fatalf("expected built-in %s to remain in defaults after Register, got %v", required, names)
		}
	}
}

func TestRegistryRegisterDoesNotBreakLookupByUnknownName(t *testing.T) {
	runtime := newRegistryForTest(t)

	runtime.registry.Register(Definition{
		Name:             "mcp_demo__echo",
		Description:      "echo",
		Parameters:       json.RawMessage(`{"type":"object"}`),
		InvocationPolicy: InvocationPolicyDiscretionary,
		ResultCharBudget: 100,
		Handler: func(_ context.Context, _ CallContext, _ json.RawMessage) (toolResult, error) {
			return toolResult{Status: "ok"}, nil
		},
	})

	_, ok := runtime.registry.Lookup("mcp_demo__does_not_exist")
	if ok {
		t.Fatalf("expected lookup of unknown name to fail")
	}
}
