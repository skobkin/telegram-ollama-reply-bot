package mcp

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
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
