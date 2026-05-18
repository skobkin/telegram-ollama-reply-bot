package tooluse

import (
	"encoding/json"
	"net/url"
	"strings"
	"unicode/utf8"

	"telegram-ollama-reply-bot/internal/llm"
)

const (
	fetchURLContentTextCharLimit     = 2600
	searchHistorySnippetCharLimit    = 280
	conversationSummaryTextCharLimit = 1400
	recentLinksSnippetCharLimit      = 180
	messageThreadSnippetCharLimit    = 220
	defaultSearchHistoryResultLimit  = 5
	maxSearchHistoryResultLimit      = 20
	defaultRecentLinksResultLimit    = 5
	maxRecentLinksResultLimit        = 10
)

func marshalResult(result toolResult, charBudget int) string {
	if charBudget <= 0 {
		charBudget = defaultToolResultCharBudget
	}

	data, err := json.Marshal(result)
	if err != nil {
		data = []byte(`{"status":"error","error":"failed to serialize tool result"}`)
	}

	return truncateUTF8(string(data), charBudget)
}

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

func extractHTTPURLs(text string) []string {
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
	if result.Summary != "" {
		fields = append(fields, "summary", result.Summary)
	}
	if result.Error != "" {
		fields = append(fields, "error", result.Error)
	}
	if errorCode, errorMessage, ok := toolErrorDetails(result); ok {
		fields = append(fields, "error_code", errorCode, "error_message", errorMessage)
	}
	fields = append(fields, attrs...)

	callCtx.Logger.Info("tool result", fields...)
}

func toolErrorDetails(result toolResult) (code, message string, ok bool) {
	data, ok := result.Data.(map[string]any)
	if !ok {
		return "", "", false
	}

	rawError, ok := data["error"]
	if !ok {
		return "", "", false
	}

	switch value := rawError.(type) {
	case toolErrorPayload:
		return value.Code, value.Message, true
	case map[string]any:
		code, _ := value["code"].(string)
		message, _ := value["message"].(string)
		if code == "" && message == "" {
			return "", "", false
		}

		return code, message, true
	default:
		return "", "", false
	}
}
