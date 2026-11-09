package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"telegram-ollama-reply-bot/internal/config"
	"telegram-ollama-reply-bot/internal/llm"
	"telegram-ollama-reply-bot/internal/state"
	"telegram-ollama-reply-bot/internal/tooluse"
)

// closedMCPEndpoint reserves a loopback port, then releases it, so the
// returned URL points at a port where connections are refused immediately
// (no listener, no timeout wait). Deterministic and fast: the TCP stack
// answers with ECONNREFUSED instead of the request ever leaving the host.
func closedMCPEndpoint(t *testing.T) string {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve local port: %v", err)
	}
	endpoint := "http://" + listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("release reserved port: %v", err)
	}

	return endpoint
}

// TestNewManagerSkipsUnreachableOptionalServer pins the optional-server
// contract: when a server marked `optional` cannot be reached, NewManager
// must keep going with a WARN naming the skipped server, and expose only the
// healthy server's tools.
func TestNewManagerSkipsUnreachableOptionalServer(t *testing.T) {
	healthy := newInProcessMCPServer(t, "fetch")

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	manager, err := NewManager(context.Background(), config.MCPConfig{
		Enabled: true,
		Servers: map[string]config.MCPServerConfig{
			"alpha": {URL: healthy, Timeout: 2 * time.Second},
			"zulu":  {URL: closedMCPEndpoint(t), Timeout: 2 * time.Second, Optional: true},
		},
	}, logger)
	if err != nil {
		t.Fatalf("NewManager must succeed when only the optional server is unreachable: %v", err)
	}
	defer manager.Close()

	got := make([]string, 0)
	for _, def := range manager.Definitions() {
		got = append(got, def.Name)
	}
	want := []string{"mcp_alpha__fetch"}
	if len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("definitions = %v, want %v", got, want)
	}

	out := buf.String()
	if !strings.Contains(out, "mcp server optional, skipping") {
		t.Fatalf("expected optional-skip WARN in log, got:\n%s", out)
	}
	if !strings.Contains(out, "server=zulu") {
		t.Fatalf("expected skipped server NAME in the WARN, got:\n%s", out)
	}
	if !strings.Contains(out, "level=WARN") {
		t.Fatalf("expected WARN level record, got:\n%s", out)
	}
	if !strings.Contains(out, "mcp server connected") || !strings.Contains(out, "server=alpha") {
		t.Fatalf("expected healthy server to connect, got:\n%s", out)
	}
}

// TestNewManagerFailsForUnreachableRequiredServer pins the required-server
// contract: the same unreachable endpoint without `optional` must abort
// startup with an actionable error naming the failing server (and the error
// must not lose the wrap chain).
func TestNewManagerFailsForUnreachableRequiredServer(t *testing.T) {
	healthy := newInProcessMCPServer(t, "fetch")

	manager, err := NewManager(context.Background(), config.MCPConfig{
		Enabled: true,
		Servers: map[string]config.MCPServerConfig{
			"alpha": {URL: healthy, Timeout: 2 * time.Second},
			"zulu":  {URL: closedMCPEndpoint(t), Timeout: 2 * time.Second},
		},
	}, discardLogger())
	if err == nil {
		if manager != nil {
			_ = manager.Close()
		}
		t.Fatalf("expected NewManager to fail for an unreachable required server")
	}
	if manager != nil {
		t.Fatalf("expected nil manager alongside the error, got %T", manager)
	}

	for _, want := range []string{`mcp server "zulu"`, "connect to"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("expected error to mention %q, got: %v", want, err)
		}
	}
}

// TestManagerCloseLeavesToolHandlersFailingCleanly pins the shutdown contract
// at the manager level: Close tears down the session, is safe to call twice,
// empties Definitions, and any tool Handler captured before the close fails
// cleanly and fast with the structured `mcp_unreachable` error the LLM can
// react to — no hang, no panic.
func TestManagerCloseLeavesToolHandlersFailingCleanly(t *testing.T) {
	manager, err := NewManager(context.Background(), config.MCPConfig{
		Enabled: true,
		Servers: map[string]config.MCPServerConfig{
			"alpha": {URL: newInProcessMCPServer(t, "echo"), Timeout: 2 * time.Second},
		},
	}, discardLogger())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	defs := manager.Definitions()
	if len(defs) != 1 {
		t.Fatalf("expected exactly one definition, got %d", len(defs))
	}
	handler := defs[0].Handler
	ctx := context.Background()

	// The session is live before the close: the tool answers normally.
	before, err := handler(ctx, tooluse.CallContext{}, nil)
	if err != nil {
		t.Fatalf("tool call before close: %v", err)
	}
	if before.Status != "ok" {
		t.Fatalf("expected status=ok before close, got %q", before.Status)
	}

	if err := manager.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	// Close is documented as idempotent.
	if err := manager.Close(); err != nil {
		t.Fatalf("second Close must be a no-op, got: %v", err)
	}
	if got := manager.Definitions(); len(got) != 0 {
		t.Fatalf("Definitions after Close = %v, want empty", got)
	}

	_, err = handler(ctx, tooluse.CallContext{}, nil)
	if err == nil {
		t.Fatalf("expected tool call to fail after Close")
	}
	if !strings.Contains(err.Error(), "mcp_unreachable") {
		t.Fatalf("expected mcp_unreachable error after Close, got: %v", err)
	}
}

// scriptLLM is a minimal llm service stub driven by a fixed script of
// responses: each Generate pops the next scripted response. It records every
// tool-role message handed back as build extras so tests can assert on the
// tool result the model would have seen.
type scriptLLM struct {
	mu          sync.Mutex
	responses   []llm.Response
	toolMessage []string
}

func (s *scriptLLM) BuildChatToolRequest(_ context.Context, _ llm.PromptScope, _ llm.ChatReplyContext, _ []llm.ToolDefinition, _ string, extras []llm.Message) (llm.Request, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, msg := range extras {
		if msg.Role == llm.RoleTool {
			s.toolMessage = append(s.toolMessage, msg.Text())
		}
	}

	return llm.Request{Feature: llm.FeatureChat}, nil
}

func (s *scriptLLM) Generate(_ context.Context, _ llm.Request) (llm.Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.responses) == 0 {
		return llm.Response{}, llm.ErrNoChoices
	}
	resp := s.responses[0]
	s.responses = s.responses[1:]

	return resp, nil
}

func (s *scriptLLM) recordedToolMessages() []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]string(nil), s.toolMessage...)
}

// scriptLLMResponse builders keep the scripted turns readable.
func toolCallResponse(id, name string) llm.Response {
	return llm.Response{
		Backend:      "test",
		FinishReason: llm.FinishReasonToolCalls,
		Message: llm.Message{
			Role: llm.RoleAssistant,
			ToolCalls: []llm.ToolCall{
				{ID: id, Name: name, Arguments: json.RawMessage(`{}`)},
			},
		},
	}
}

func textResponse(text string) llm.Response {
	return llm.Response{
		Backend:      "test",
		FinishReason: llm.FinishReasonStop,
		Message:      llm.TextMessage(llm.RoleAssistant, text),
	}
}

// TestRuntimeCloseClosesAttachedMCPManager pins the wiring app.Run relies on
// (tools.AttachMCP("mcp", mcpManager) + defer tools.Close): while the runtime
// is alive the attached manager's tools answer through the live session, and
// once Runtime.Close has run the registered closer the same tool call fails
// cleanly with `mcp_unreachable` instead of panicking or hanging.
func TestRuntimeCloseClosesAttachedMCPManager(t *testing.T) {
	manager, err := NewManager(context.Background(), config.MCPConfig{
		Enabled: true,
		Servers: map[string]config.MCPServerConfig{
			"alpha": {URL: newInProcessMCPServer(t, "echo"), Timeout: 2 * time.Second},
		},
	}, discardLogger())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	stub := &scriptLLM{
		responses: []llm.Response{
			toolCallResponse("call-1", "mcp_alpha__echo"),
			textResponse("first reply"),
			toolCallResponse("call-2", "mcp_alpha__echo"),
			textResponse("second reply"),
		},
	}
	runtime := tooluse.New(stub, nil, nil, nil, nil, nil, discardLogger(), tooluse.Config{})
	runtime.AttachMCP("mcp", manager)

	req := tooluse.ChatRequest{
		Scope:        state.ConversationScope{ChatID: 1},
		PromptScope:  llm.PromptScope{ChatID: 1},
		ReplyContext: llm.ChatReplyContext{UserMessage: llm.TextMessage(llm.RoleUser, "hi")},
	}

	// While the session is open, the tool loop executes the MCP tool normally.
	reply, _, err := runtime.ReplyWithTools(context.Background(), req)
	if err != nil {
		t.Fatalf("first ReplyWithTools: %v", err)
	}
	if reply != "first reply" {
		t.Fatalf("unexpected first reply: %q", reply)
	}
	if msgs := stub.recordedToolMessages(); len(msgs) != 1 || strings.Contains(msgs[0], "mcp_unreachable") {
		t.Fatalf("expected one healthy tool result before Close, got %q", msgs)
	}

	// app.Run does `defer tools.Close()`: the runtime closer must reach the
	// manager and tear its sessions down.
	runtime.Close()
	if got := manager.Definitions(); len(got) != 0 {
		t.Fatalf("manager Definitions after Runtime.Close = %v, want empty", got)
	}

	// A subsequent tool call through the (still registered) definition fails
	// cleanly; the tool loop turns it into an error tool result instead of
	// crashing, and the reply completes.
	reply, _, err = runtime.ReplyWithTools(context.Background(), req)
	if err != nil {
		t.Fatalf("second ReplyWithTools: %v", err)
	}
	if reply != "second reply" {
		t.Fatalf("unexpected second reply: %q", reply)
	}
	msgs := stub.recordedToolMessages()
	if len(msgs) != 2 {
		t.Fatalf("expected two tool results in total, got %d: %q", len(msgs), msgs)
	}
	if !strings.Contains(msgs[1], "mcp_unreachable") {
		t.Fatalf("expected post-close tool result to carry mcp_unreachable, got: %s", msgs[1])
	}

	// Repeated shutdown must stay safe: manager Close is idempotent and the
	// runtime has no closers left.
	if err := manager.Close(); err != nil {
		t.Fatalf("manager Close after Runtime.Close: %v", err)
	}
	runtime.Close()
}
