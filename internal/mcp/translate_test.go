package mcp

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"telegram-ollama-reply-bot/internal/config"
	"telegram-ollama-reply-bot/internal/tooluse"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// validToolNameRe mirrors the OpenAI-compatible function-name constraint in
// its canonical lower-case form (after sanitization).
var validToolNameRe = regexp.MustCompile(`^[a-z0-9_-]+$`)

// validHashRe matches the 8-hex truncation suffix appended to capped names.
var validHashRe = regexp.MustCompile(`^[0-9a-f]{8}$`)

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

func TestCheckToolNameCollisionsRejectsCollidingRawNames(t *testing.T) {
	err := checkToolNameCollisions([]string{"search.web", "search-web", "search_web"})
	if err == nil {
		t.Fatalf("expected error for colliding tool names")
	}
	for _, want := range []string{"search.web", "search-web", "search_web"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("expected error to mention %q, got: %v", want, err)
		}
	}
}

func TestCheckToolNameCollisionsAcceptsDistinctNames(t *testing.T) {
	err := checkToolNameCollisions([]string{"search.web", "search_news", "fetch"})
	if err != nil {
		t.Fatalf("unexpected error for distinct names: %v", err)
	}
}

func TestCheckToolNameCollisionsHandlesEmptyList(t *testing.T) {
	if err := checkToolNameCollisions(nil); err != nil {
		t.Fatalf("unexpected error for empty list: %v", err)
	}
}

// TestNamespacedToolNameCapsLongNamesAtSixtyFourCharacters pins the F10
// contract: a sanitized name longer than the OpenAI-compatible 64-char
// function-name limit is truncated with a deterministic hex hash suffix, so
// the chat request is not rejected with a 400.
func TestNamespacedToolNameCapsLongNamesAtSixtyFourCharacters(t *testing.T) {
	longTool := strings.Repeat("supabase_", 12) + "migration"

	got := namespacedToolName("supabase", longTool)
	if len(got) > 64 {
		t.Fatalf("capped name length = %d, want <= 64 (%q)", len(got), got)
	}
	if !validToolNameRe.MatchString(got) {
		t.Fatalf("capped name %q does not match ^[a-z0-9_-]+$", got)
	}
	if again := namespacedToolName("supabase", longTool); again != got {
		t.Fatalf("truncation is not deterministic: %q vs %q", got, again)
	}

	// Format: readable 55-char prefix of the untruncated sanitized name,
	// then '-' plus 8 hex chars.
	untruncated := "mcp_supabase__" + sanitizeToolNameSegment(longTool)
	if got[:55] != untruncated[:55] {
		t.Fatalf("readable prefix = %q, want %q", got[:55], untruncated[:55])
	}
	if got[55] != '-' {
		t.Fatalf("expected '-' separator at index 55, got %q", string(got[55]))
	}
	if !validHashRe.MatchString(got[56:]) {
		t.Fatalf("expected 8-hex hash suffix, got %q", got[56:])
	}
}

// TestNamespacedToolNameTruncationDistinguishesLongPrefixCollisions ensures
// two distinct raw tools whose sanitized names share the whole readable
// prefix still get distinct final names via the hash suffix.
func TestNamespacedToolNameTruncationDistinguishesLongPrefixCollisions(t *testing.T) {
	first := namespacedToolName("supabase", strings.Repeat("very_long_tool_name_", 5)+"alpha")
	second := namespacedToolName("supabase", strings.Repeat("very_long_tool_name_", 5)+"beta")

	if first == second {
		t.Fatalf("distinct raw tools collapsed onto one truncated name %q", first)
	}
	if len(first) > 64 || len(second) > 64 {
		t.Fatalf("truncated names exceed 64 chars: %d, %d", len(first), len(second))
	}
}

// TestNamespacedToolNameKeepsSixtyFourCharNameUnchanged pins the boundary:
// exactly 64 sanitized characters pass through unmodified (no hash suffix).
func TestNamespacedToolNameKeepsSixtyFourCharNameUnchanged(t *testing.T) {
	server := strings.Repeat("s", 10)
	tool := strings.Repeat("t", 64-len(mcpToolNamePrefix)-len(server)-len(mcpToolNameSep)) // 48 -> total 64

	want := mcpToolNamePrefix + server + mcpToolNameSep + tool
	got := namespacedToolName(server, tool)
	if len(want) != 64 {
		t.Fatalf("test setup: untruncated length = %d, want 64", len(want))
	}
	if got != want {
		t.Fatalf("boundary name was altered: got %q, want %q", got, want)
	}
}

// TestNamespacedToolNameTruncatesSixtyFiveCharName checks the first
// character beyond the limit triggers truncation.
func TestNamespacedToolNameTruncatesSixtyFiveCharName(t *testing.T) {
	server := strings.Repeat("s", 10)
	tool := strings.Repeat("t", 49) // 4+10+2+49 = 65

	got := namespacedToolName(server, tool)
	if len(got) != 64 {
		t.Fatalf("truncated length = %d, want 64", len(got))
	}
	if got[:len(mcpToolNamePrefix)+len(server)] != mcpToolNamePrefix+server {
		t.Fatalf("truncated name lost its server prefix: %q", got)
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

// TestSchemaAsParametersFallsBackForNonObjectRoots pins the JSON-Schema-object
// contract: the OpenAI-compatible chat API expects `parameters` to be a JSON
// Schema OBJECT, so a tool whose advertised schema has a non-object JSON root
// (legal in draft 2020-12) must fall back to `{"type":"object"}` instead of
// being forwarded verbatim and breaking every tool call with an API-side 400.
// Object roots are forwarded unchanged.
func TestSchemaAsParametersFallsBackForNonObjectRoots(t *testing.T) {
	const fallback = `{"type":"object"}`

	cases := []struct {
		name  string
		input any
		want  string
	}{
		{"string root", "string", fallback},
		{"number root", 3.5, fallback},
		{"array root", []any{"a"}, fallback},
		{"bool root", true, fallback},
		{"null root", json.RawMessage(`null`), fallback},
		{"object root forwarded", map[string]any{"type": "object"}, `{"type":"object"}`},
		{"object schema forwarded", json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}}}`), `{"type":"object","properties":{"q":{"type":"string"}}}`},
		{"nil input", nil, fallback},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := schemaAsParameters(tc.input)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if string(got) != tc.want {
				t.Fatalf("schemaAsParameters(%v) = %s, want %s", tc.input, string(got), tc.want)
			}
		})
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
