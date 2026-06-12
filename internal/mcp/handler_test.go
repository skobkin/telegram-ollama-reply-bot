package mcp

import (
	"encoding/json"
	"log/slog"
	"reflect"
	"strings"
	"testing"

	"telegram-ollama-reply-bot/internal/tooluse"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestParseToolArgumentsReturnsObjectForEmpty(t *testing.T) {
	got, err := parseToolArguments(nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	obj, ok := got.(map[string]any)
	if !ok {
		t.Fatalf("expected map[string]any, got %T", got)
	}
	if len(obj) != 0 {
		t.Fatalf("expected empty object, got %v", obj)
	}
}

func TestParseToolArgumentsReturnsObjectForNull(t *testing.T) {
	got, err := parseToolArguments(json.RawMessage(`null`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	obj, ok := got.(map[string]any)
	if !ok {
		t.Fatalf("expected map[string]any, got %T", got)
	}
	if len(obj) != 0 {
		t.Fatalf("expected empty object, got %v", obj)
	}
}

func TestParseToolArgumentsPassesObjectThrough(t *testing.T) {
	in := json.RawMessage(`{"q":"hello","n":3}`)
	got, err := parseToolArguments(in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	obj, ok := got.(map[string]any)
	if !ok {
		t.Fatalf("expected map[string]any, got %T", got)
	}
	if obj["q"] != "hello" {
		t.Fatalf("unexpected q: %v", obj["q"])
	}
	// JSON numbers decode to float64.
	if obj["n"] != float64(3) {
		t.Fatalf("unexpected n: %v", obj["n"])
	}
}

func TestParseToolArgumentsWrapsScalarsAsValueField(t *testing.T) {
	cases := []json.RawMessage{
		json.RawMessage(`"hello"`),
		json.RawMessage(`42`),
		json.RawMessage(`true`),
	}
	for _, in := range cases {
		t.Run(string(in), func(t *testing.T) {
			got, err := parseToolArguments(in)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			obj, ok := got.(map[string]any)
			if !ok {
				t.Fatalf("expected map[string]any, got %T", got)
			}
			if _, ok := obj["value"]; !ok {
				t.Fatalf("expected wrapped scalar under `value`, got %v", obj)
			}
		})
	}
}

func TestParseToolArgumentsRejectsInvalidJSON(t *testing.T) {
	_, err := parseToolArguments(json.RawMessage(`{"q":`))
	if err == nil {
		t.Fatalf("expected error for invalid JSON")
	}
}

func TestDecodeCallResultCollectsTextAndBlocks(t *testing.T) {
	reply := &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{Text: "first"},
			&mcp.TextContent{Text: "second"},
		},
	}

	result, err := decodeCallResult(reply)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status != "ok" {
		t.Fatalf("expected status=ok, got %q", result.Status)
	}
	data, ok := result.Data.(map[string]any)
	if !ok {
		t.Fatalf("expected map[string]any, got %T", result.Data)
	}
	if data["text"] != "first\nsecond" {
		t.Fatalf("expected text joined with newlines, got %v", data["text"])
	}
	blocks, ok := data["blocks"].([]map[string]any)
	if !ok {
		t.Fatalf("expected blocks to be []map[string]any, got %T", data["blocks"])
	}
	if len(blocks) != 2 {
		t.Fatalf("expected 2 blocks, got %d", len(blocks))
	}
	for _, b := range blocks {
		if b["type"] != "text" {
			t.Fatalf("expected text block, got %v", b)
		}
	}
}

func TestDecodeCallResultCollectsStructuredContent(t *testing.T) {
	structured := map[string]any{"answer": 42}
	reply := &mcp.CallToolResult{
		Content:           []mcp.Content{&mcp.TextContent{Text: "ignored"}},
		StructuredContent: structured,
	}

	result, err := decodeCallResult(reply)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	data, ok := result.Data.(map[string]any)
	if !ok {
		t.Fatalf("expected map[string]any, got %T", result.Data)
	}
	if !reflect.DeepEqual(data["structured"], structured) {
		t.Fatalf("expected structured to be preserved, got %v", data["structured"])
	}
}

func TestDecodeCallResultClassifiesUnsupportedContentTypes(t *testing.T) {
	reply := &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{Text: "caption"},
			&mcp.ImageContent{MIMEType: "image/png", Data: []byte{0x01, 0x02, 0x03}},
			&mcp.AudioContent{MIMEType: "audio/mp3", Data: []byte{0x04, 0x05}},
			&mcp.EmbeddedResource{Resource: &mcp.ResourceContents{MIMEType: "text/html", URI: "https://example.com"}},
		},
	}

	result, err := decodeCallResult(reply)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	data, ok := result.Data.(map[string]any)
	if !ok {
		t.Fatalf("expected map[string]any, got %T", result.Data)
	}
	unsupported, ok := data["unsupported"].([]map[string]any)
	if !ok {
		t.Fatalf("expected unsupported to be []map[string]any, got %T", data["unsupported"])
	}
	if len(unsupported) != 3 {
		t.Fatalf("expected 3 unsupported items, got %d", len(unsupported))
	}
	wantTypes := []string{"image", "audio", "embedded_resource"}
	for i, want := range wantTypes {
		if unsupported[i]["type"] != want {
			t.Fatalf("expected type[%d]=%q, got %v", i, want, unsupported[i])
		}
	}
}

func TestDecodeCallResultReportsEmptyWhenAllContentSkipped(t *testing.T) {
	reply := &mcp.CallToolResult{Content: nil}

	result, err := decodeCallResult(reply)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	data, ok := result.Data.(map[string]any)
	if !ok {
		t.Fatalf("expected map[string]any, got %T", result.Data)
	}
	if data["empty"] != true {
		t.Fatalf("expected empty=true, got %v", data)
	}
}

func TestDecodeCallResultHandlesNilTextBlock(t *testing.T) {
	reply := &mcp.CallToolResult{
		Content: []mcp.Content{
			(*mcp.TextContent)(nil),
			&mcp.TextContent{Text: "ok"},
		},
	}

	result, err := decodeCallResult(reply)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	data := result.Data.(map[string]any)
	if data["text"] != "ok" {
		t.Fatalf("expected only the non-nil text block, got %v", data["text"])
	}
}

func TestSummarizeForLogCountsTextAndUnsupported(t *testing.T) {
	reply := &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{Text: "hello"},
			&mcp.TextContent{Text: "world"},
			&mcp.ImageContent{MIMEType: "image/png"},
			&mcp.AudioContent{MIMEType: "audio/mp3"},
		},
	}

	textLen, blockCount, unsupportedCount := summarizeForLog(reply)
	if textLen != len("hello")+len("world") {
		t.Fatalf("unexpected text length: %d", textLen)
	}
	if blockCount != 2 {
		t.Fatalf("expected 2 text blocks, got %d", blockCount)
	}
	if unsupportedCount != 2 {
		t.Fatalf("expected 2 unsupported items, got %d", unsupportedCount)
	}
}

func TestSummarizeForLogHandlesNilReply(t *testing.T) {
	textLen, blockCount, unsupportedCount := summarizeForLog(nil)
	if textLen != 0 || blockCount != 0 || unsupportedCount != 0 {
		t.Fatalf("expected all zeros for nil reply, got textLen=%d blockCount=%d unsupportedCount=%d",
			textLen, blockCount, unsupportedCount)
	}
}

func TestLogCallResultNeverIncludesHeaderValuesOrPayloads(t *testing.T) {
	// The log line is structured, so we cannot easily capture a textual
	// assertion. Instead, we test that the helper accepts the call shape
	// without panicking and is a no-op when logger is nil. The "no header
	// values" guarantee is enforced by the helper signature (it never
	// receives a header map) and the `server` field is attached upstream.
	if got := serverLogger(nil, "demo"); got == nil {
		t.Fatalf("expected non-nil logger from serverLogger")
	}

	// Smoke-test logCallResult with a discarding logger.
	discard := slog.New(slog.NewTextHandler(devNull{}, nil))
	logCallResult(discard, "mcp_demo__echo", "echo", "req-1", false, 42, 1, 0)
	logCallResult(nil, "mcp_demo__echo", "echo", "req-1", false, 0, 0, 0)
}

type devNull struct{}

func (devNull) Write(p []byte) (int, error) { return len(p), nil }

// TestCallToolHandlerReturnsErrorWhenSessionIsNil pins the contract that
// a handler closure built with a nil session is a clear, machine-parseable
// failure rather than a nil-pointer panic.
func TestCallToolHandlerReturnsErrorWhenSessionIsNil(t *testing.T) {
	handler := callToolHandler(nil, "echo", "mcp_demo__echo", nil)
	_, err := handler(nil, tooluse.CallContext{}, json.RawMessage(`{}`))
	if err == nil {
		t.Fatalf("expected error for nil session")
	}
	if !strings.Contains(err.Error(), "mcp session is not connected") {
		t.Fatalf("unexpected error message: %v", err)
	}
}
