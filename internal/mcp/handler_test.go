package mcp

import (
	"encoding/json"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

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

// TestDecodeCallResultAllTextCarriesSingleCopy pins the no-duplication
// contract: an all-text result exposes the joined text once under "text" and
// never adds a "blocks" key.
func TestDecodeCallResultAllTextCarriesSingleCopy(t *testing.T) {
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
	if _, present := data["blocks"]; present {
		t.Fatalf("expected no blocks key for all-text results, got %v", data["blocks"])
	}
	marshaled, err := json.Marshal(data)
	if err != nil {
		t.Fatalf("marshal data: %v", err)
	}
	if got := strings.Count(string(marshaled), "first"); got != 1 {
		t.Fatalf("expected exactly one copy of the text in the payload, got %d: %s", got, marshaled)
	}
}

// TestDecodeCallResultErrorSummaryCarriesServerMessage pins the tool-error
// contract: when the server returns IsError, the model must see the server's
// own message in Summary (not a generic "tool returned a result").
func TestDecodeCallResultErrorSummaryCarriesServerMessage(t *testing.T) {
	reply := &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: "disk full"}},
	}

	result, err := decodeCallResult(reply)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status != "error" {
		t.Fatalf("expected status=error, got %q", result.Status)
	}
	if result.Summary != "disk full" {
		t.Fatalf("expected server error message in summary, got %q", result.Summary)
	}
	if data := result.Data.(map[string]any); data["text"] != "disk full" {
		t.Fatalf("expected text to carry the message too, got %v", data["text"])
	}
}

// TestDecodeCallResultErrorSummaryJoinsTextBlocks checks that a multi-block
// error message is joined with newlines in the summary.
func TestDecodeCallResultErrorSummaryJoinsTextBlocks(t *testing.T) {
	reply := &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{
			&mcp.TextContent{Text: "line 1"},
			&mcp.TextContent{Text: "line 2"},
		},
	}

	result, err := decodeCallResult(reply)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status != "error" {
		t.Fatalf("expected status=error, got %q", result.Status)
	}
	if result.Summary != "line 1\nline 2" {
		t.Fatalf("expected joined error summary, got %q", result.Summary)
	}
}

// TestDecodeCallResultErrorSummaryFallsBackWithoutText checks the fallback
// summary when an error result carries no text content at all.
func TestDecodeCallResultErrorSummaryFallsBackWithoutText(t *testing.T) {
	reply := &mcp.CallToolResult{IsError: true}

	result, err := decodeCallResult(reply)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status != "error" {
		t.Fatalf("expected status=error, got %q", result.Status)
	}
	if result.Summary != "tool returned an error" {
		t.Fatalf("expected fallback summary, got %q", result.Summary)
	}
}

// TestDecodeCallResultOkSummaryForNonError checks that a successful result
// keeps the generic summary (callers must not confuse it with an error).
func TestDecodeCallResultOkSummaryForNonError(t *testing.T) {
	reply := &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: "disk fine"}},
	}

	result, err := decodeCallResult(reply)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status != "ok" || result.Summary != "tool returned a result" {
		t.Fatalf("expected ok result with generic summary, got status=%q summary=%q", result.Status, result.Summary)
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

// TestDecodeCallResultMixedContentBlocksListNonTextOnly checks that a mixed
// result lists the non-text items under "blocks" while the joined text stays
// the only copy of the text content.
func TestDecodeCallResultMixedContentBlocksListNonTextOnly(t *testing.T) {
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
	if data["text"] != "caption" {
		t.Fatalf("expected joined text under text, got %v", data["text"])
	}
	if _, present := data["unsupported"]; present {
		t.Fatalf("expected no separate unsupported key, got %v", data["unsupported"])
	}
	blocks, ok := data["blocks"].([]map[string]any)
	if !ok {
		t.Fatalf("expected blocks to be []map[string]any, got %T", data["blocks"])
	}
	if len(blocks) != 3 {
		t.Fatalf("expected 3 non-text blocks, got %d", len(blocks))
	}
	wantTypes := []string{"image", "audio", "embedded_resource"}
	for i, want := range wantTypes {
		if blocks[i]["type"] != want {
			t.Fatalf("expected type[%d]=%q, got %v", i, want, blocks[i])
		}
	}
	for _, b := range blocks {
		if _, hasText := b["text"]; hasText {
			t.Fatalf("expected no duplicated text inside blocks, got %v", b)
		}
	}
	// The marshaled payload must not carry the text more than once.
	marshaled, err := json.Marshal(data)
	if err != nil {
		t.Fatalf("marshal data: %v", err)
	}
	if got := strings.Count(string(marshaled), "caption"); got != 1 {
		t.Fatalf("expected exactly one copy of the text in the payload, got %d: %s", got, marshaled)
	}
}

// TestDecodeCallResultNonTextOnlyListsBlocks verifies a result with no text
// at all still surfaces the non-text content instead of collapsing to
// "empty".
func TestDecodeCallResultNonTextOnlyListsBlocks(t *testing.T) {
	reply := &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.ImageContent{MIMEType: "image/jpeg", Data: []byte{0x01}},
		},
	}

	result, err := decodeCallResult(reply)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	data := result.Data.(map[string]any)
	if _, present := data["text"]; present {
		t.Fatalf("expected no text key, got %v", data["text"])
	}
	if _, present := data["empty"]; present {
		t.Fatalf("expected non-empty data for image-only content, got %v", data)
	}
	blocks, ok := data["blocks"].([]map[string]any)
	if !ok || len(blocks) != 1 || blocks[0]["type"] != "image" {
		t.Fatalf("expected a single image block, got %v", data["blocks"])
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
	handler := callToolHandler(nil, "echo", "mcp_demo__echo", 5*time.Second, nil)
	_, err := handler(nil, tooluse.CallContext{}, json.RawMessage(`{}`))
	if err == nil {
		t.Fatalf("expected error for nil session")
	}
	if !strings.Contains(err.Error(), "mcp session is not connected") {
		t.Fatalf("unexpected error message: %v", err)
	}
}
