package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"telegram-ollama-reply-bot/internal/config"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// discardLogger returns a logger that swallows output, for tests that do not
// assert on log records.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newInProcessMCPServer starts an MCP server on a local httptest listener
// advertising one tool per given name (each backed by a stub handler that
// returns a static text result). The listener is closed on test cleanup.
func newInProcessMCPServer(t *testing.T, toolNames ...string) string {
	t.Helper()

	server := mcp.NewServer(&mcp.Implementation{Name: "test-server", Version: "0.0.0"}, nil)
	for _, name := range toolNames {
		tool := &mcp.Tool{
			Name:        name,
			Description: "test tool " + name,
			InputSchema: json.RawMessage(`{"type":"object"}`),
		}
		server.AddTool(tool, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: "ok"}},
			}, nil
		})
	}

	httpServer := httptest.NewServer(mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return server },
		nil,
	))
	t.Cleanup(httpServer.Close)

	return httpServer.URL
}

func TestSortedServerNamesReturnsKeysSorted(t *testing.T) {
	servers := map[string]config.MCPServerConfig{
		"zulu":  {URL: "http://zulu"},
		"alpha": {URL: "http://alpha"},
		"mike":  {URL: "http://mike"},
	}

	got := sortedServerNames(servers)
	want := []string{"alpha", "mike", "zulu"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sortedServerNames = %v, want %v", got, want)
	}
}

func TestSortedServerNamesHandlesEmptyConfig(t *testing.T) {
	if got := sortedServerNames(nil); len(got) != 0 {
		t.Fatalf("expected empty slice for nil map, got %v", got)
	}
}

// TestNewManagerRegistersServersInSortedOrder wires three servers registered
// out of order in the config map and asserts that the ordered output of
// NewManager (Definitions) is sorted by server name, so the tool list given
// to the LLM is deterministic across restarts.
func TestNewManagerRegistersServersInSortedOrder(t *testing.T) {
	endpoints := map[string]string{
		"zulu":  newInProcessMCPServer(t, "zulu_tool"),
		"alpha": newInProcessMCPServer(t, "alpha_tool"),
		"mike":  newInProcessMCPServer(t, "mike_tool"),
	}

	servers := make(map[string]config.MCPServerConfig, len(endpoints))
	for name, endpoint := range endpoints {
		servers[name] = config.MCPServerConfig{
			URL:     endpoint,
			Timeout: 2 * time.Second,
		}
	}

	manager, err := NewManager(context.Background(), config.MCPConfig{
		Enabled: true,
		Servers: servers,
	}, discardLogger())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	defer manager.Close()

	got := make([]string, 0)
	for _, def := range manager.Definitions() {
		got = append(got, def.Name)
	}
	want := []string{
		"mcp_alpha__alpha_tool",
		"mcp_mike__mike_tool",
		"mcp_zulu__zulu_tool",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("definitions order = %v, want %v", got, want)
	}
}

// TestNewManagerWarnsWhenDisabledWithConfiguredServers pins the
// misconfiguration hint: MCP disabled by config while servers are configured
// must WARN with the configured server NAMES (never URLs), while the
// disabled-and-empty case keeps the plain INFO.
func TestNewManagerWarnsWhenDisabledWithConfiguredServers(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	manager, err := NewManager(context.Background(), config.MCPConfig{
		Enabled: false,
		Servers: map[string]config.MCPServerConfig{
			"supabase": {
				URL:     "http://127.0.0.1:9/harbour",
				Headers: map[string]string{"Authorization": "Bearer super-secret-token"},
			},
			"atlas": {URL: "http://127.0.0.1:9/atlas"},
		},
	}, logger)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if err := manager.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "mcp support disabled by config while servers are configured") {
		t.Fatalf("expected misconfiguration WARN in log, got:\n%s", out)
	}
	if !strings.Contains(out, "supabase") || !strings.Contains(out, "atlas") {
		t.Fatalf("expected configured server names in the WARN, got:\n%s", out)
	}
	if strings.Contains(out, "harbour") || strings.Contains(out, "127.0.0.1") {
		t.Fatalf("server URL leaked into log:\n%s", out)
	}
	if !strings.Contains(out, "level=WARN") {
		t.Fatalf("expected WARN level record, got:\n%s", out)
	}
	if strings.Contains(out, "level=INFO") {
		t.Fatalf("disabled-with-servers case must not log the plain INFO:\n%s", out)
	}
}

// TestNewManagerLogsPlainInfoWhenDisabledAndEmpty keeps the quiet path: no
// servers configured plus MCP disabled is the normal off state and must not
// produce the misconfiguration WARN.
func TestNewManagerLogsPlainInfoWhenDisabledAndEmpty(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	manager, err := NewManager(context.Background(), config.MCPConfig{Enabled: false}, logger)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if err := manager.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "mcp support disabled by config") {
		t.Fatalf("expected plain disabled INFO in log, got:\n%s", out)
	}
	if strings.Contains(out, "while servers are configured") {
		t.Fatalf("unexpected misconfiguration WARN for empty config:\n%s", out)
	}
}

// TestNewManagerRejectsCollidingToolNamesOnOneServer pins the fail-fast
// behavior for two remote tools whose sanitized names collapse onto one
// namespaced name: boot must error instead of silently registering whichever
// tool happens to come last.
func TestNewManagerRejectsCollidingToolNamesOnOneServer(t *testing.T) {
	endpoint := newInProcessMCPServer(t, "search.web", "search-web")

	_, err := NewManager(context.Background(), config.MCPConfig{
		Enabled: true,
		Servers: map[string]config.MCPServerConfig{
			"srv": {URL: endpoint, Timeout: 2 * time.Second},
		},
	}, discardLogger())
	if err == nil {
		t.Fatalf("expected NewManager to fail on colliding tool names")
	}

	for _, want := range []string{"srv", "search.web", "search-web", "search_web"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("expected error to mention %q, got: %v", want, err)
		}
	}
}

// TestNewManagerRejectsCollidingServerNames pins the fail-fast behavior for
// two servers whose sanitized names collapse onto the same prefix: both
// tools would land on one namespaced name and the winner would depend on map
// iteration order.
func TestNewManagerRejectsCollidingServerNames(t *testing.T) {
	first := newInProcessMCPServer(t, "fetch")
	second := newInProcessMCPServer(t, "fetch")

	_, err := NewManager(context.Background(), config.MCPConfig{
		Enabled: true,
		Servers: map[string]config.MCPServerConfig{
			"a-b":  {URL: first, Timeout: 2 * time.Second},
			"a__b": {URL: second, Timeout: 2 * time.Second},
		},
	}, discardLogger())
	if err == nil {
		t.Fatalf("expected NewManager to fail on colliding server names")
	}

	for _, want := range []string{"a-b", "a__b", "mcp_a_b__fetch"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("expected error to mention %q, got: %v", want, err)
		}
	}
}

// TestNewManagerAcceptsDistinctToolNamesAcrossServers keeps the healthy path
// working: same tool name on differently-named servers must not be treated
// as a collision.
func TestNewManagerAcceptsDistinctToolNamesAcrossServers(t *testing.T) {
	servers := map[string]config.MCPServerConfig{
		"alpha": {URL: newInProcessMCPServer(t, "fetch"), Timeout: 2 * time.Second},
		"beta":  {URL: newInProcessMCPServer(t, "fetch"), Timeout: 2 * time.Second},
	}

	manager, err := NewManager(context.Background(), config.MCPConfig{
		Enabled: true,
		Servers: servers,
	}, discardLogger())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	defer manager.Close()

	got := make([]string, 0)
	for _, def := range manager.Definitions() {
		got = append(got, def.Name)
	}
	want := []string{"mcp_alpha__fetch", "mcp_beta__fetch"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("definitions = %v, want %v", got, want)
	}
}
