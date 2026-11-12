package mcp

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"unicode"

	"telegram-ollama-reply-bot/internal/config"
	"telegram-ollama-reply-bot/internal/tooluse"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	// mcpToolNamePrefix is prepended to every translated tool name so the LLM
	// (and human operators reading logs) can tell at a glance which tools come
	// from a remote MCP server and which server they belong to.
	mcpToolNamePrefix = "mcp_"
	mcpToolNameSep    = "__"

	// mcpToolNameMaxLen caps the final namespaced name at 64 characters.
	// OpenAI-compatible backends constrain function names to
	// ^[a-zA-Z0-9_-]{1,64}$, and a longer name gets the entire chat request
	// rejected with a 400 — not just the tool call.
	mcpToolNameMaxLen = 64

	// mcpToolNameHashLen is the length of the hex sha256 suffix appended to
	// truncated names (see namespacedNameHashSuffix).
	mcpToolNameHashLen = 8

	// defaultMCPResultCharBudget is the per-tool result character budget used
	// when an operator has not set one. The fetch-url budget is a reasonable
	// starting point: large enough to fit a typical tool payload, small enough
	// to keep the LLM context bounded.
	defaultMCPResultCharBudget = 3500
)

// sanitizeToolNameSegment lower-cases, strips stray C0 control characters
// (defensive against misbehaving servers), and replaces every rune that is
// not [a-z0-9_] with an underscore. Empty segments become "x". The result is
// safe to embed in the `mcp_<server>__<tool>` namespaced identifier.
func sanitizeToolNameSegment(s string) string {
	s = stripNulAndControl(s)
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return "x"
	}
	runes := make([]rune, 0, len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
			runes = append(runes, r)
		case r >= '0' && r <= '9':
			runes = append(runes, r)
		case r == '_':
			runes = append(runes, r)
		default:
			runes = append(runes, '_')
		}
	}
	// Collapse runs of underscores that can result from punctuation such as
	// "my-server.tool" -> "my_server_tool".
	return collapseUnderscores(string(runes))
}

func collapseUnderscores(s string) string {
	if !strings.Contains(s, "__") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	prevUnderscore := false
	for _, r := range s {
		if r == '_' {
			if prevUnderscore {

				continue
			}
			prevUnderscore = true
			b.WriteRune(r)

			continue
		}
		prevUnderscore = false
		b.WriteRune(r)
	}

	return b.String()
}

// namespacedToolName returns the bot's name for an MCP tool: "mcp_<server>__<tool>".
// Sanitizing both segments independently makes collisions with built-in tooluse
// tool names (no underscores between segments) and with each other visible at
// the call site.
//
// The result is capped at mcpToolNameMaxLen characters: when the sanitized
// name is longer, the readable part is truncated and a "-" plus a short hex
// sha256 prefix of the full unsanitized name is appended (e.g.
// "mcp_supabase…-1a2b3c4d"), so the result stays deterministic,
// collision-resistant, and within the limit.
func namespacedToolName(serverName, toolName string) string {
	full := mcpToolNamePrefix + sanitizeToolNameSegment(serverName) + mcpToolNameSep + sanitizeToolNameSegment(toolName)
	if len(full) <= mcpToolNameMaxLen {
		return full
	}

	suffix := namespacedNameHashSuffix(serverName, toolName)
	keep := mcpToolNameMaxLen - 1 - len(suffix)

	return full[:keep] + "-" + suffix
}

// namespacedNameHashSuffix derives the short hex hash identifying the full
// unsanitized namespaced name. It is appended to truncated names so distinct
// tools whose sanitized names share a long prefix stay distinguishable, and
// truncation stays deterministic across restarts.
func namespacedNameHashSuffix(serverName, toolName string) string {
	raw := mcpToolNamePrefix + serverName + mcpToolNameSep + toolName
	sum := sha256.Sum256([]byte(raw))

	return hex.EncodeToString(sum[:])[:mcpToolNameHashLen]
}

// checkToolNameCollisions rejects a set of advertised tool names when two of
// them sanitize onto the same tool-name segment (e.g. "search.web" and
// "search-web"): Registry.Register would silently replace the earlier
// definition with the later one, and which one wins would depend on map
// iteration order. The error names both raw tools so the operator can rename
// one or scope it out of the filters.
func checkToolNameCollisions(names []string) error {
	seen := make(map[string]string, len(names))
	for _, raw := range names {
		segment := sanitizeToolNameSegment(raw)
		if prev, ok := seen[segment]; ok {
			return fmt.Errorf("tool name collision: %q and %q both translate to the same tool name %q", prev, raw, segment)
		}
		seen[segment] = raw
	}

	return nil
}

// resolveInvocationPolicy returns the operator override if set, otherwise
// derives a sensible default from the MCP tool's `readOnlyHint` annotation.
// The returned string is one of the values accepted by tooluse.InvocationPolicy.
func resolveInvocationPolicy(server config.MCPServerConfig, annotations *mcp.ToolAnnotations) tooluse.InvocationPolicy {
	if policy := config.InvocationPolicyFor(server.InvocationPolicy); policy != "" {
		return tooluse.InvocationPolicy(policy)
	}
	if annotations != nil && annotations.ReadOnlyHint {
		return tooluse.InvocationPolicyDiscretionary
	}

	return tooluse.InvocationPolicyExplicitRequestOnly
}

// resolveSideEffecting returns the operator override if set, otherwise derives
// from the MCP tool's `readOnlyHint` and `destructiveHint` annotations.
//
//   - Read-only tools are always non-side-effecting (destructiveHint is only
//     meaningful when ReadOnlyHint == false per the MCP spec).
//   - Otherwise, a nil `destructiveHint` is treated conservatively as `true`,
//     matching the MCP specification default.
func resolveSideEffecting(server config.MCPServerConfig, annotations *mcp.ToolAnnotations) bool {
	if server.SideEffecting != nil {
		return *server.SideEffecting
	}
	if annotations != nil && annotations.ReadOnlyHint {
		return false
	}
	if annotations == nil || annotations.DestructiveHint == nil {
		return true
	}

	return *annotations.DestructiveHint
}

// resolveResultCharBudget returns the per-tool result character budget, using
// the operator override when set and a sensible default otherwise.
func resolveResultCharBudget(server config.MCPServerConfig) int {
	if server.ResultCharBudget > 0 {
		return server.ResultCharBudget
	}

	return defaultMCPResultCharBudget
}

// schemaAsParameters re-marshals the tool's input schema to JSON so it can be
// passed straight to the LLM as `tooluse.Definition.Parameters`. The MCP SDK
// guarantees the value marshals to valid JSON, but the OpenAI-compatible chat
// API expects `parameters` to be a JSON Schema OBJECT — so anything else at
// the JSON root falls back to `{"type":"object"}`: nil/empty/null schemas
// (so the model can still call the tool with no arguments), plus the bare
// "string", number, boolean, and array roots that are legal in draft 2020-12
// and would otherwise break every tool call with an API-side 400.
func schemaAsParameters(schema any) (json.RawMessage, error) {
	if schema == nil {
		return json.RawMessage(`{"type":"object"}`), nil
	}
	data, err := json.Marshal(schema)
	if err != nil {
		return nil, fmt.Errorf("marshal schema: %w", err)
	}
	if !isJSONObjectRoot(data) {
		return json.RawMessage(`{"type":"object"}`), nil
	}

	return data, nil
}

// isJSONObjectRoot reports whether the JSON document in data has an object at
// its root: the first non-whitespace byte must open an object. Empty input is
// not an object root.
func isJSONObjectRoot(data []byte) bool {
	for _, b := range data {
		switch b {
		case ' ', '\t', '\n', '\r':
			continue
		case '{':
			return true
		default:

			return false
		}
	}

	return false
}

// toolAnnotations returns the MCP SDK tool's annotation block, or nil when
// the server did not provide any. It exists so tests can stub a tool without
// importing the SDK.
func toolAnnotations(tool *mcp.Tool) *mcp.ToolAnnotations {
	if tool == nil {
		return nil
	}

	return tool.Annotations
}

// translateDefinition converts one MCP tool into a tooluse.Definition that can
// be registered on the existing tooluse.Registry. The Handler field is left
// nil — the caller (Manager) wires the call closure so that invocation
// routes back to the originating server.
func translateDefinition(serverName string, server config.MCPServerConfig, tool *mcp.Tool) (tooluse.Definition, error) {
	if tool == nil {
		return tooluse.Definition{}, fmt.Errorf("nil tool for server %q", serverName)
	}

	rawName := strings.TrimSpace(tool.Name)
	if rawName == "" {
		return tooluse.Definition{}, fmt.Errorf("server %q advertised a tool with an empty name", serverName)
	}

	parameters, err := schemaAsParameters(tool.InputSchema)
	if err != nil {
		return tooluse.Definition{}, fmt.Errorf("server %q tool %q: %w", serverName, rawName, err)
	}

	annotations := toolAnnotations(tool)

	return tooluse.Definition{
		Name:             namespacedToolName(serverName, rawName),
		Description:      tool.Description,
		Parameters:       parameters,
		InvocationPolicy: resolveInvocationPolicy(server, annotations),
		ResultCharBudget: resolveResultCharBudget(server),
		SideEffecting:    resolveSideEffecting(server, annotations),
		Visibility:       tooluse.VisibilityStandard,
	}, nil
}

// logTranslatedDefinition records one structured log line per translated tool
// at INFO level. Header values are never present in these fields.
func logTranslatedDefinition(logger *slog.Logger, serverName, originalName, namespacedName string, definition tooluse.Definition) {
	if logger == nil {
		return
	}
	logger.Info(
		"mcp tool registered",
		"server", serverName,
		"original_name", originalName,
		"tool_name", namespacedName,
		"invocation_policy", string(definition.InvocationPolicy),
		"side_effecting", definition.SideEffecting,
		"result_char_budget", definition.ResultCharBudget,
	)
}

// stripNulAndControl removes any C0 control characters that may have leaked
// into a tool name from a misbehaving server. Defensive: MCP servers are
// supposed to restrict names to a sane character set, but a stray byte should
// not break the LLM prompt.
func stripNulAndControl(s string) string {
	if !strings.ContainsFunc(s, func(r rune) bool {
		return r == 0 || (unicode.IsControl(r) && r != '\t')
	}) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r == 0 || (unicode.IsControl(r) && r != '\t') {
			continue
		}
		b.WriteRune(r)
	}

	return b.String()
}
