package bot

import (
	"context"
	"strings"
	"testing"
	"time"

	"telegram-ollama-reply-bot/internal/adminconfig"
	"telegram-ollama-reply-bot/internal/support/markdown"
	"telegram-ollama-reply-bot/internal/tooluse"

	tg "github.com/mymmrac/telego"
)

func historyCalls() []tooluse.ToolCallRecord {
	return []tooluse.ToolCallRecord{
		{CallID: "1", Name: "get_conversation_summary", Visibility: tooluse.VisibilityInternal, Status: "ok", Duration: 300 * time.Millisecond},
		{CallID: "2", Name: "search_web", Visibility: tooluse.VisibilityStandard, Status: "ok", Duration: time.Second},
		{CallID: "3", Name: "fetch_url_content", Visibility: tooluse.VisibilityStandard, Status: "error", Duration: 800 * time.Millisecond},
	}
}

func TestRichToolHistoryBlockToolsMode(t *testing.T) {
	block := renderRichToolHistoryBlock(historyCalls(), adminconfig.PreviewModeTools, RichMessageCharLimit)

	// Standard tools only, no durations.
	if !strings.Contains(block, "✅ `search_web()`") || strings.Contains(block, "⌛") {
		t.Fatalf("tools mode must list standard tools without durations: %q", block)
	}
	if strings.Contains(block, "get_conversation_summary") {
		t.Fatalf("internal tools must be hidden in tools mode: %q", block)
	}
	if !strings.Contains(block, "❌ `fetch_url_content()`") {
		t.Fatalf("failed tools must be marked: %q", block)
	}
	if !strings.Contains(block, "<details><summary>Tool calls (2)</summary>") || !strings.HasSuffix(block, "</details>") {
		t.Fatalf("block shape unexpected: %q", block)
	}
}

func TestRichToolHistoryBlockAllToolsMode(t *testing.T) {
	block := renderRichToolHistoryBlock(historyCalls(), adminconfig.PreviewModeAllTools, RichMessageCharLimit)

	if !strings.Contains(block, "✅ `get_conversation_summary()` ⌛ 0.3s") {
		t.Fatalf("all_tools mode must include internal tools with durations: %q", block)
	}
	if !strings.Contains(block, "✅ `search_web()` ⌛ 1s") {
		t.Fatalf("all_tools mode must carry durations: %q", block)
	}
	if !strings.Contains(block, "Tool calls (3)") {
		t.Fatalf("summary must count all visible calls: %q", block)
	}
}

func TestRichToolHistoryBlockBudget(t *testing.T) {
	calls := historyCalls()

	// No room: the whole block is omitted.
	if block := renderRichToolHistoryBlock(calls, adminconfig.PreviewModeTools, 0); block != "" {
		t.Fatalf("no budget must omit the block: %q", block)
	}
	if block := renderRichToolHistoryBlock(nil, adminconfig.PreviewModeTools, RichMessageCharLimit); block != "" {
		t.Fatalf("no visible calls must omit the block: %q", block)
	}

	// Tight budget: durations dropped before entries.
	plain := renderRichToolHistoryBlock(calls, adminconfig.PreviewModeAllTools, 130)
	if strings.Contains(plain, "⌛") {
		t.Fatalf("tight budget must drop durations first: %q", plain)
	}
	if !strings.Contains(plain, "Tool calls (3)") {
		t.Fatalf("tight budget must keep all entries: %q", plain)
	}

	// Tighter budget: oldest entries compacted away with a more line — one
	// rune below the full plain block forces compaction.
	plainFull := renderRichToolHistoryBlock(calls, adminconfig.PreviewModeTools, 1<<20)
	compacted := renderRichToolHistoryBlock(calls, adminconfig.PreviewModeTools, len([]rune(plainFull))-1)
	if !strings.Contains(compacted, "… and ") || strings.Contains(compacted, "search_web()") {
		t.Fatalf("tighter budget must compact the oldest entries: %q", compacted)
	}
}

// TestSendReplyWithToolHistoryAnswerFirst pins the answer-first guarantee:
// the answer portion on the wire is exactly the answer cropped to its own
// rich limit, identical to a send without history — the block only fills
// the remaining capacity and never causes extra truncation.
func TestSendReplyWithToolHistoryAnswerFirst(t *testing.T) {
	state := newPreviewServerState()
	b := newPreviewTestBot(t, state)

	long := strings.Repeat("word ", 9000) // ~45k runes, over the rich limit
	wantAnswer, _ := markdown.CropRichMarkdown(long, RichMessageCharLimit)

	sent, err := b.sendReplyWithToolHistory(context.Background(), tg.Message{MessageID: 9, Chat: tg.Chat{ID: 55}},
		long, historyCalls(), adminconfig.PreviewModeTools)
	if err != nil {
		t.Fatalf("send with history: %v", err)
	}
	if sent == nil || sent.MessageID == 0 {
		t.Fatalf("expected a sent message, got %+v", sent)
	}

	body := state.calls("sendRichMessage")[0]["rich_message"].(map[string]any)["markdown"].(string)
	// The cropped answer fills the whole limit, leaving no capacity for the
	// block: it is omitted, and the answer is identical to a send without
	// history — neither truncated further nor overflowing the limit.
	if body != wantAnswer {
		t.Fatal("answer portion must be identical to a send without history, block omitted when there is no room")
	}
	if len([]rune(body)) > RichMessageCharLimit {
		t.Fatalf("wire payload must not exceed the rich limit: %d", len([]rune(body)))
	}
}

func TestSendReplyWithToolHistoryShortAnswer(t *testing.T) {
	state := newPreviewServerState()
	b := newPreviewTestBot(t, state)

	_, err := b.sendReplyWithToolHistory(context.Background(), tg.Message{MessageID: 9, Chat: tg.Chat{ID: 55}},
		"Here is the answer.", historyCalls(), adminconfig.PreviewModeAllTools)
	if err != nil {
		t.Fatalf("send with history: %v", err)
	}

	body := state.calls("sendRichMessage")[0]["rich_message"].(map[string]any)["markdown"].(string)
	if body != "Here is the answer."+renderRichToolHistoryBlock(historyCalls(), adminconfig.PreviewModeAllTools, RichMessageCharLimit) {
		t.Fatalf("wire payload unexpected: %q", body)
	}
}

func TestSendReplyWithToolHistoryLegacyFallback(t *testing.T) {
	state := newPreviewServerState()
	b := newPreviewTestBot(t, state)
	state.armFailure("sendRichMessage", 1)

	_, err := b.sendReplyWithToolHistory(context.Background(), tg.Message{MessageID: 9, Chat: tg.Chat{ID: 55}},
		"Here is the answer.", historyCalls(), adminconfig.PreviewModeAllTools)
	if err != nil {
		t.Fatalf("fallback send: %v", err)
	}

	fallback := state.calls("sendMessage")[0]
	if fallback["parse_mode"] != "MarkdownV2" {
		t.Fatalf("fallback must use MarkdownV2: %v", fallback)
	}
	text := fallback["text"].(string)
	if strings.Contains(text, "<details>") || strings.Contains(text, "`") {
		t.Fatalf("fallback must not carry details markup or code spans: %q", text)
	}
	// The suffix reaches the wire in its sanitized (escaped) form: raw
	// MarkdownV2-reserved parentheses would get the whole fallback
	// rejected. (Underscores may pass through as sanitizer italic pairs —
	// valid MarkdownV2, same treatment as the answer body.)
	if !strings.Contains(text, "Tool calls \\(3\\):") || !strings.Contains(text, "get_conversation_summary") {
		t.Fatalf("fallback suffix must list all tools in all_tools mode: %q", text)
	}
	// Every parenthesis must be escaped — a raw one would get the whole
	// fallback rejected.
	if strings.Count(text, "(") != strings.Count(text, "\\(") {
		t.Fatalf("raw parentheses must not reach the MarkdownV2 wire: %q", text)
	}
	if !strings.Contains(text, "Here is the answer") {
		t.Fatalf("answer must survive the fallback: %q", text)
	}
}

func TestPlainToolHistorySuffixBudget(t *testing.T) {
	calls := historyCalls()
	sanitize := func(s string) string { return s }

	if suffix := renderPlainToolHistorySuffix(sanitize, calls, adminconfig.PreviewModeTools, 200); suffix == "" {
		t.Fatal("suffix must render with room")
	}
	if suffix := renderPlainToolHistorySuffix(sanitize, calls, adminconfig.PreviewModeTools, 0); suffix != "" {
		t.Fatalf("no budget must omit the suffix: %q", suffix)
	}

	// Compaction: entries drop until the sanitized form fits — one rune
	// below the full suffix forces the "more" line.
	full := renderPlainToolHistorySuffix(sanitize, calls, adminconfig.PreviewModeTools, 1<<20)
	tight := renderPlainToolHistorySuffix(sanitize, calls, adminconfig.PreviewModeTools, len([]rune(full))-1)
	if !strings.Contains(tight, "… and ") || len([]rune(tight)) > len([]rune(full)) {
		t.Fatalf("tight budget must compact with a more line: %q (full %q)", tight, full)
	}

	// Sanitized length is what is measured: a sanitizer that escapes must
	// push the suffix over budget instead of overflowing the limit.
	escaper := func(s string) string { return strings.ReplaceAll(s, "_", "\\_") }
	if suffix := renderPlainToolHistorySuffix(escaper, historyCalls(), adminconfig.PreviewModeTools, 25); suffix != "" {
		t.Fatalf("escaping must be measured before fitting: %q", suffix)
	}
}

// TestSendReplyWithToolHistoryCodeFenceAnswer pins the blank-line
// separation between the answer and the history block: a trailing fenced
// code block keeps its closing fence on a line of its own, and the details
// block starts as block-level HTML.
func TestSendReplyWithToolHistoryCodeFenceAnswer(t *testing.T) {
	state := newPreviewServerState()
	b := newPreviewTestBot(t, state)

	answer := "```go\nfmt.Println(1)\n```"
	_, err := b.sendReplyWithToolHistory(context.Background(), tg.Message{MessageID: 9, Chat: tg.Chat{ID: 55}},
		answer, historyCalls(), adminconfig.PreviewModeTools)
	if err != nil {
		t.Fatalf("send with history: %v", err)
	}

	body := state.calls("sendRichMessage")[0]["rich_message"].(map[string]any)["markdown"].(string)
	want := answer + renderRichToolHistoryBlock(historyCalls(), adminconfig.PreviewModeTools, RichMessageCharLimit-len([]rune(answer)))
	if body != want {
		t.Fatalf("fence-ending answer must keep the block separated: %q", body)
	}
	if !strings.Contains(body, "```\n\n<details>") {
		t.Fatalf("block must start on its own line: %q", body)
	}
}

// TestPlainToolHistorySuffixReturnsSanitizedForm pins that the fallback
// suffix is returned in its sanitized form: the renderer measures the
// escaped length, so what it returns must be exactly that escaped text.
func TestPlainToolHistorySuffixReturnsSanitizedForm(t *testing.T) {
	sanitizer := markdown.NewTgMarkdownV2Sanitizer()
	suffix := renderPlainToolHistorySuffix(sanitizer.Sanitize, historyCalls(), adminconfig.PreviewModeTools, 1<<20)
	if !strings.Contains(suffix, "Tool calls \\(2\\):") {
		t.Fatalf("suffix must be the escaped wire form: %q", suffix)
	}
	// Every parenthesis must be escaped — a raw one would get the whole
	// fallback rejected.
	if strings.Count(suffix, "(") != strings.Count(suffix, "\\(") {
		t.Fatalf("raw suffix must not be returned: %q", suffix)
	}
}

func TestVisibleToolCallsFiltering(t *testing.T) {
	all := visibleToolCalls(historyCalls(), adminconfig.PreviewModeAllTools)
	if len(all) != 3 {
		t.Fatalf("all_tools must show every call, got %d", len(all))
	}

	standard := visibleToolCalls(historyCalls(), adminconfig.PreviewModeTools)
	if len(standard) != 2 {
		t.Fatalf("tools mode must hide internal calls, got %d", len(standard))
	}

	// Unknown visibility resolves to standard (present in tools mode).
	unknown := visibleToolCalls([]tooluse.ToolCallRecord{{Name: "x", Visibility: "", Status: "ok"}}, adminconfig.PreviewModeTools)
	if len(unknown) != 1 {
		t.Fatalf("unknown visibility must stay visible, got %d", len(unknown))
	}
}
