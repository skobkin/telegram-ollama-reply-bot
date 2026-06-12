package mcp

import (
	"encoding/json"
	"strings"
	"testing"

	"telegram-ollama-reply-bot/internal/config"
	"telegram-ollama-reply-bot/internal/tooluse"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestSanitizeToolNameSegmentLowercasesAndReplacesPunctuation(t *testing.T) {
	cases := map[string]string{
		"browser":        "browser",
		"Browser":        "browser",
		"my-server.tool": "my_server_tool",
		"  spaced  ":     "spaced",
		"":               "x",
		"already_ok_123": "already_ok_123",
		"a..b..c":        "a_b_c",
	}

	for in, want := range cases {
		t.Run(in, func(t *testing.T) {
			if got := sanitizeToolNameSegment(in); got != want {
				t.Fatalf("sanitizeToolNameSegment(%q) = %q, want %q", in, got, want)
			}
		})
	}
}

func TestNamespacedToolNameProducesMcpPrefix(t *testing.T) {
	got := namespacedToolName("browser", "fetch_url")
	want := "mcp_browser__fetch_url"
	if got != want {
		t.Fatalf("namespacedToolName = %q, want %q", got, want)
	}
}

func TestTranslateDefinitionDefaultsFromAnnotations(t *testing.T) {
	readOnly := true
	tool := &mcp.Tool{
		Name:        "fetch",
		Description: "fetch a URL",
		InputSchema: json.RawMessage(`{"type":"object"}`),
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: readOnly},
	}

	def, err := translateDefinition("browser", config.MCPServerConfig{}, tool)
	if err != nil {
		t.Fatalf("translateDefinition: %v", err)
	}
	if def.Name != "mcp_browser__fetch" {
		t.Fatalf("unexpected name: %q", def.Name)
	}
	if def.InvocationPolicy != tooluse.InvocationPolicyDiscretionary {
		t.Fatalf("expected discretionary for read-only, got %q", def.InvocationPolicy)
	}
	if def.SideEffecting {
		t.Fatalf("expected non-side-effecting for read-only, got true")
	}
	if def.ResultCharBudget != defaultMCPResultCharBudget {
		t.Fatalf("unexpected result char budget: %d", def.ResultCharBudget)
	}
}

func TestTranslateDefinitionUsesExplicitRequestOnlyWhenNoReadOnlyHint(t *testing.T) {
	tool := &mcp.Tool{
		Name:        "post",
		Description: "post data",
		InputSchema: json.RawMessage(`{"type":"object"}`),
	}

	def, err := translateDefinition("api", config.MCPServerConfig{}, tool)
	if err != nil {
		t.Fatalf("translateDefinition: %v", err)
	}
	if def.InvocationPolicy != tooluse.InvocationPolicyExplicitRequestOnly {
		t.Fatalf("unexpected invocation policy: %q", def.InvocationPolicy)
	}
	if !def.SideEffecting {
		t.Fatalf("expected side-effecting when destructiveHint is unset")
	}
}

func TestTranslateDefinitionHonoursOperatorOverrides(t *testing.T) {
	tool := &mcp.Tool{
		Name:        "search",
		Description: "search the web",
		InputSchema: json.RawMessage(`{"type":"object"}`),
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}
	sideEffecting := false
	def, err := translateDefinition("api", config.MCPServerConfig{
		InvocationPolicy: config.InvocationPolicyDiscretionaryPaid,
		SideEffecting:    &sideEffecting,
		ResultCharBudget: 1234,
	}, tool)
	if err != nil {
		t.Fatalf("translateDefinition: %v", err)
	}
	if def.InvocationPolicy != tooluse.InvocationPolicyDiscretionaryPaid {
		t.Fatalf("unexpected invocation policy: %q", def.InvocationPolicy)
	}
	if def.SideEffecting {
		t.Fatalf("expected operator override of side_effecting to win")
	}
	if def.ResultCharBudget != 1234 {
		t.Fatalf("unexpected result char budget: %d", def.ResultCharBudget)
	}
}

func TestTranslateDefinitionRejectsNilTool(t *testing.T) {
	_, err := translateDefinition("api", config.MCPServerConfig{}, nil)
	if err == nil {
		t.Fatalf("expected error for nil tool")
	}
}

func TestTranslateDefinitionRejectsEmptyName(t *testing.T) {
	tool := &mcp.Tool{Name: "   ", Description: "x"}
	_, err := translateDefinition("api", config.MCPServerConfig{}, tool)
	if err == nil {
		t.Fatalf("expected error for empty tool name")
	}
}

func TestSchemaAsParametersFallsBackToObjectOnEmpty(t *testing.T) {
	got, err := schemaAsParameters(nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(string(got), `"object"`) {
		t.Fatalf("expected fallback object schema, got %s", string(got))
	}
}

func TestSchemaAsParametersPassesThroughJSONSchema(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}}}`)
	got, err := schemaAsParameters(schema)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(got) != string(schema) {
		t.Fatalf("expected passthrough, got %s", string(got))
	}
}

func TestResolveInvocationPolicyUnknownOperatorValueFallsBackToAnnotation(t *testing.T) {
	server := config.MCPServerConfig{InvocationPolicy: "unknown_policy"}
	got := resolveInvocationPolicy(server, &mcp.ToolAnnotations{ReadOnlyHint: true})
	if got != tooluse.InvocationPolicyDiscretionary {
		t.Fatalf("expected discretionary from annotation, got %q", got)
	}
}

func TestResolveSideEffectingDefaultsToTrueWhenAnnotationMissing(t *testing.T) {
	got := resolveSideEffecting(config.MCPServerConfig{}, nil)
	if !got {
		t.Fatalf("expected side_effecting=true by default")
	}
}
