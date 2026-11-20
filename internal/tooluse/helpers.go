package tooluse

import (
	"net/url"
	"strings"
	"unicode/utf8"

	"telegram-ollama-reply-bot/internal/llm"
)

const (
	fetchURLContentTextCharLimit    = 2600
	searchHistorySnippetCharLimit   = 280
	recentLinksSnippetCharLimit     = 180
	messageThreadSnippetCharLimit   = 220
	defaultSearchHistoryResultLimit = 5
	maxSearchHistoryResultLimit     = 20
	defaultRecentLinksResultLimit   = 5
	maxRecentLinksResultLimit       = 10
)

func truncateUTF8(text string, limit int) string {
	if limit <= 0 || utf8.RuneCountInString(text) <= limit {
		return text
	}

	runes := []rune(text)
	if limit <= 1 {
		return string(runes[:limit])
	}

	return string(runes[:limit-1]) + "…"
}

func accumulateUsage(total *llm.TokenUsage, next llm.TokenUsage) {
	total.PromptTokens += next.PromptTokens
	total.CompletionTokens += next.CompletionTokens
	total.TotalTokens += next.TotalTokens
	total.Cost += next.Cost
}

func usagePointer(usage llm.TokenUsage) *llm.TokenUsage {
	if usage == (llm.TokenUsage{}) {
		return nil
	}

	return &usage
}

func normalizeWhitespace(text string) string {
	return strings.Join(strings.Fields(text), " ")
}

// ExtractHTTPURLs returns the unique http(s) URLs mentioned in a message
// text. Candidates are whitespace-separated fields with surrounding
// punctuation trimmed; every returned URL has passed scheme validation and
// round-trips through url.ParseRequestURI. Shared by the link-scanning tools
// and the auto-summarizer.
func ExtractHTTPURLs(text string) []string {
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return nil
	}

	result := make([]string, 0, len(fields))
	for _, field := range fields {
		candidate := strings.TrimSpace(field)
		candidate = strings.Trim(candidate, `"'()[]{}<>.,;:!?`)
		if !isValidToolURL(candidate) {
			continue
		}

		parsed, err := url.ParseRequestURI(candidate)
		if err != nil {
			continue
		}

		result = append(result, parsed.String())
	}

	return result
}

func logToolResult(callCtx CallContext, result toolResult, attrs ...any) {
	if callCtx.Logger == nil {
		return
	}

	fields := make([]any, 0, len(attrs)+10)
	fields = append(fields,
		"status", result.Status,
		"topic_id", callCtx.Scope.TopicID,
	)

	result = normalizeResult(result)
	if result.Error != nil {
		fields = append(fields, "error_code", result.Error.Code, "error_class", result.Error.Class, "retryable", result.Error.Retryable)
	}

	fields = append(fields, attrs...)

	callCtx.Logger.Info("tool result", fields...)
}
