package bot

import (
	"context"
	"fmt"
	"strings"

	"telegram-ollama-reply-bot/internal/adminconfig"
	"telegram-ollama-reply-bot/internal/execution"
	"telegram-ollama-reply-bot/internal/support/markdown"
	"telegram-ollama-reply-bot/internal/tooluse"

	t "github.com/mymmrac/telego"
	tu "github.com/mymmrac/telego/telegoutil"
)

// toolHistorySummary is the shared summary line prefix of the collapsible
// tool-history block (rich) and the plain fallback suffix.
const toolHistorySummary = "Tool calls"

// sendReplyWithToolHistory sends the final answer with an appended
// collapsible tool-history block built from the run's authoritative tool
// call records. Budgeting is answer-first: the answer is cropped only to
// its own existing rich-message limit — exactly as it would be without
// history — and the block is compacted into the remaining capacity,
// omitted entirely when there is no room. Diagnostics never cause
// additional truncation of an otherwise valid answer.
//
// The rich send falls back to the legacy MarkdownV2 pipeline on failure;
// the fallback appends a plain single-line suffix (no <details> markup,
// no code spans) only if room remains within the plain character limit.
func (b *Bot) sendReplyWithToolHistory(ctx context.Context, message t.Message, answer string, calls []tooluse.ToolCallRecord, mode adminconfig.PreviewMode) (*t.Message, error) {
	logger := b.loggerFromContext(ctx)

	body := answer
	if cropped, changed := markdown.CropRichMarkdown(body, RichMessageCharLimit); changed {
		logger.Warn("rich reply cropped to fit length limit",
			"original_length", len([]rune(body)), "cropped_length", len([]rune(cropped)))
		body = cropped
	}

	block := renderRichToolHistoryBlock(calls, mode, RichMessageCharLimit-len([]rune(body)))

	sent, err := b.api.SendRichMessage(ctx, buildRichMessageParams(tu.ID(message.Chat.ID), message, body+block))
	if err == nil {
		return sent, nil
	}

	logger.Warn("cannot send rich message, falling back to MarkdownV2",
		"error", err, "reply_length", len([]rune(body))+len([]rune(block)))

	// Legacy fallback: the answer is sanitized and cropped exactly as it is
	// without history; the suffix must fit what remains of the plain limit.
	sanitized := b.sanitizer.Sanitize(answer)
	cropped, changed := markdown.CropToMaxLengthMarkdownV2(sanitized, TelegramCharLimit)
	if changed {
		cropped = b.sanitizer.Sanitize(cropped)
	}

	suffix := renderPlainToolHistorySuffix(b.sanitizer.Sanitize, calls, mode, TelegramCharLimit-len([]rune(cropped)))

	return b.api.SendMessage(ctx, b.reply(message,
		tu.Message(tu.ID(message.Chat.ID), cropped+suffix).WithParseMode(t.ModeMarkdownV2)))
}

// renderRichToolHistoryBlock renders the collapsible rich-markdown block
// within maxRunes, or "" when no visible calls exist or nothing fits.
// Compaction order: durations are dropped first, then the oldest entries
// (summarized in an "and N more" line); the whole block is the last resort.
func renderRichToolHistoryBlock(calls []tooluse.ToolCallRecord, mode adminconfig.PreviewMode, maxRunes int) string {
	visible := visibleToolCalls(calls, mode)
	if len(visible) == 0 {
		return ""
	}

	build := func(lines []string) string {
		var b strings.Builder
		fmt.Fprintf(&b, "<details><summary>%s (%d)</summary>\n\n", toolHistorySummary, len(visible))
		for _, line := range lines {
			b.WriteString(line + "\n")
		}
		b.WriteString("\n</details>")

		return b.String()
	}

	withDurations := toolHistoryLines(visible, mode == adminconfig.PreviewModeAllTools)
	if len([]rune(build(withDurations))) <= maxRunes {
		return build(withDurations)
	}

	plain := toolHistoryLines(visible, false)
	if len([]rune(build(plain))) <= maxRunes {
		return build(plain)
	}

	for keep := len(visible) - 1; keep >= 1; keep-- {
		compacted := make([]string, 0, keep+1)
		compacted = append(compacted, fmt.Sprintf("… and %d more", len(visible)-keep))
		compacted = append(compacted, toolHistoryLines(visible[len(visible)-keep:], false)...)
		if len([]rune(build(compacted))) <= maxRunes {
			return build(compacted)
		}
	}

	return "" // no room: omit the block entirely, the answer stands alone
}

// renderPlainToolHistorySuffix renders the MarkdownV2 fallback suffix — a
// single plain line without details markup or code spans — within maxRunes,
// measured on the sanitized form (escaping lengthens text). Returns ""
// when there are no visible calls or nothing fits.
func renderPlainToolHistorySuffix(sanitize func(string) string, calls []tooluse.ToolCallRecord, mode adminconfig.PreviewMode, maxRunes int) string {
	visible := visibleToolCalls(calls, mode)
	if len(visible) == 0 {
		return ""
	}

	build := func(records []tooluse.ToolCallRecord, omitted int) string {
		var b strings.Builder
		fmt.Fprintf(&b, "%s (%d):", toolHistorySummary, len(visible))
		for _, r := range records {
			b.WriteString(" " + toolHistoryLine(r, false, mode == adminconfig.PreviewModeAllTools))
		}
		if omitted > 0 {
			fmt.Fprintf(&b, " … and %d more", omitted)
		}

		return "\n" + b.String()
	}

	if len([]rune(sanitize(build(visible, 0)))) <= maxRunes {
		return build(visible, 0)
	}

	for keep := len(visible) - 1; keep >= 1; keep-- {
		compacted := build(visible[len(visible)-keep:], len(visible)-keep)
		if len([]rune(sanitize(compacted))) <= maxRunes {
			return compacted
		}
	}

	return ""
}

// visibleToolCalls filters records by preview mode: all tools in all_tools,
// standard tools only otherwise.
func visibleToolCalls(calls []tooluse.ToolCallRecord, mode adminconfig.PreviewMode) []tooluse.ToolCallRecord {
	if mode == adminconfig.PreviewModeAllTools {
		return calls
	}

	visible := make([]tooluse.ToolCallRecord, 0, len(calls))
	for _, call := range calls {
		if call.Visibility != tooluse.VisibilityInternal {
			visible = append(visible, call)
		}
	}

	return visible
}

// toolHistoryLines renders one line per record; durations appear only in
// all_tools mode.
func toolHistoryLines(records []tooluse.ToolCallRecord, durations bool) []string {
	lines := make([]string, 0, len(records))
	for _, record := range records {
		lines = append(lines, toolHistoryLine(record, true, durations))
	}

	return lines
}

// toolHistoryLine renders one tool-history entry. Rich output puts the name
// into a code span (escaped when it could break the span); plain output
// needs no markup and is sanitized as a whole by the caller.
func toolHistoryLine(record tooluse.ToolCallRecord, rich bool, duration bool) string {
	mark := "✅"
	if record.Status != execution.StatusOK {
		mark = "❌"
	}

	line := mark + " " + previewToolName(record.Name, rich)
	if duration && record.Duration > 0 {
		line += " ⌛ " + formatShortDuration(record.Duration)
	}

	return line
}
