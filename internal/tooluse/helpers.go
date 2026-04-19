package tooluse

import (
	"encoding/json"
	"unicode/utf8"

	"telegram-ollama-reply-bot/internal/llm"
)

const (
	defaultToolResultCharBudget = 1800

	fetchURLContentResultCharBudget     = 3500
	searchRecentHistoryResultCharBudget = 2200
	conversationSummaryResultCharBudget = 1800
	createPollResultCharBudget          = 1200
	reminderStubResultCharBudget        = 900
	fetchURLContentTextCharLimit        = 2600
	searchRecentHistorySnippetCharLimit = 280
	conversationSummaryTextCharLimit    = 1400
	defaultRecentHistoryResultLimit     = 5
	maxRecentHistoryResultLimit         = 10
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
