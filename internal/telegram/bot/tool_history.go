package bot

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"telegram-ollama-reply-bot/internal/adminconfig"
	"telegram-ollama-reply-bot/internal/execution"
	"telegram-ollama-reply-bot/internal/support/markdown"
	"telegram-ollama-reply-bot/internal/support/safeerr"
	"telegram-ollama-reply-bot/internal/tooluse"

	t "github.com/mymmrac/telego"
	tu "github.com/mymmrac/telego/telegoutil"
)

// toolHistorySummary is the shared summary line prefix of the collapsible
// tool-history block (rich) and the plain fallback suffix.
const toolHistorySummary = "Tool calls"

// stripToolHistoryPresentation removes the presentation-only tool-history
// metadata this package appends to final replies — the rich <details> block
// and the plain fallback suffix — from the rendered wire text of a quoted
// bot message, so the stored quote keeps its subject without re-importing
// diagnostics. Both anchor forms are matched (the plain suffix is
// MarkdownV2-sanitized on the wire, escaping its parentheses), at the last
// occurrence: the block is always appended at the very end of an answer.
// Text without a block is returned unchanged; a deliberately crafted
// suffix look-alike is stripped toward safety.
func stripToolHistoryPresentation(text string) string {
	anchors := []string{
		"\n\n<details><summary>" + toolHistorySummary + " (",
		"\n" + toolHistorySummary + " \\(",
		"\n" + toolHistorySummary + " (",
	}

	cut := len(text)
	for _, anchor := range anchors {
		if at := strings.LastIndex(text, anchor); at >= 0 && at < cut {
			cut = at
		}
	}
	if cut == len(text) {
		return text
	}

	// The separators preceding a block or suffix line are presentation
	// spacing: blank runs left before the cut go with the block.
	return strings.TrimRight(text[:cut], " \n\t")
}

// errRichHistorySend names rich-send failures of the history pipeline for
// logging. Raw transport errors are never surfaced: the request URL embeds
// the bot token, so causes are discarded via safeerr.
var errRichHistorySend = errors.New("rich reply send failed")

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
	if isRateLimit(err) {
		// A 429 is chat-wide and transient: the legacy fallback walks into
		// the same rate-limited window, so it is not attempted. The rate
		// limit is returned unchanged so the caller's gate logic can absorb
		// it — flattening it into a fallback error would hide the signal.
		return nil, err
	}

	logger.Warn("cannot send rich message, falling back to MarkdownV2",
		"error", safeerr.WrapTransport(errRichHistorySend, err),
		"reply_length", len([]rune(body))+len([]rune(block)))

	// Legacy fallback: the answer is sanitized and cropped exactly as it is
	// without history; the suffix must fit what remains of the plain limit.
	sanitized := b.sanitizer.Sanitize(answer)
	cropped, changed := markdown.CropToMaxLengthMarkdownV2(sanitized, TelegramCharLimit)
	if changed {
		cropped = b.sanitizer.Sanitize(cropped)
	}

	suffix := renderPlainToolHistorySuffix(b.sanitizer.Sanitize, calls, mode, TelegramCharLimit-len([]rune(cropped)))

	// The fallback preserves the originating forum topic exactly like the
	// rich path and the ordinary fallback do.
	params := b.reply(message,
		tu.Message(tu.ID(message.Chat.ID), cropped+suffix).WithParseMode(t.ModeMarkdownV2))
	if message.MessageThreadID != 0 {
		params.MessageThreadID = message.MessageThreadID
	}

	return b.api.SendMessage(ctx, params)
}

// renderRichToolHistoryBlock renders the collapsible rich-markdown block
// within maxRunes, or "" when no visible calls exist or nothing fits. The
// block starts with a blank line: the separator keeps it out of a trailing
// fenced code block of the answer (a closing fence must stand alone) and
// makes the HTML block-level, and its runes come out of the block's own
// budget. Compaction order: durations are dropped first, then the oldest
// entries (summarized in an "and N more" line); the whole block is the last
// resort.
func renderRichToolHistoryBlock(calls []tooluse.ToolCallRecord, mode adminconfig.PreviewMode, maxRunes int) string {
	visible := visibleToolCalls(calls, mode)
	if len(visible) == 0 {
		return ""
	}

	build := func(lines []string) string {
		var b strings.Builder
		fmt.Fprintf(&b, "\n\n<details><summary>%s (%d)</summary>\n\n", toolHistorySummary, len(visible))
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
// single plain line without details markup or code spans — within maxRunes.
// The returned suffix is the sanitized form itself (escaping lengthens
// text, so the fit is measured on exactly what is sent): a raw suffix could
// carry unescaped MarkdownV2-reserved characters and get the whole legacy
// fallback rejected. Returns "" when there are no visible calls or nothing
// fits.
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

	if sanitized := sanitize(build(visible, 0)); len([]rune(sanitized)) <= maxRunes {
		return sanitized
	}

	for keep := len(visible) - 1; keep >= 1; keep-- {
		sanitized := sanitize(build(visible[len(visible)-keep:], len(visible)-keep))
		if len([]rune(sanitized)) <= maxRunes {
			return sanitized
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
