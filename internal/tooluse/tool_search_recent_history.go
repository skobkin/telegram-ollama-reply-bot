package tooluse

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"telegram-ollama-reply-bot/internal/llmcontext"
	"telegram-ollama-reply-bot/internal/state"
)

func (r *Runtime) searchHistory(_ context.Context, callCtx CallContext, args json.RawMessage) (toolResult, error) {
	var payload struct {
		Query string `json:"query"`
		Limit int    `json:"limit"`
	}
	if err := json.Unmarshal(args, &payload); err != nil && string(args) != "" && string(args) != "null" {
		return toolResult{}, fmt.Errorf("parse arguments: %w", err)
	}
	if payload.Limit <= 0 {
		payload.Limit = defaultRecentHistoryResultLimit
	}
	if payload.Limit > maxRecentHistoryResultLimit {
		payload.Limit = maxRecentHistoryResultLimit
	}

	snapshot := r.history.Snapshot(callCtx.Scope)
	matches := make([]map[string]any, 0, payload.Limit)
	query := strings.TrimSpace(strings.ToLower(payload.Query))

	for i := len(snapshot.Messages) - 1; i >= 0 && len(matches) < payload.Limit; i-- {
		msg := snapshot.Messages[i]
		candidate := strings.ToLower(llmcontext.RenderMessagesPlainText([]state.Message{msg}))
		if query != "" && !strings.Contains(candidate, query) {
			continue
		}

		matches = append(matches, map[string]any{
			"name":       msg.Name,
			"username":   msg.Username,
			"text":       truncateUTF8(strings.TrimSpace(msg.Text), searchRecentHistorySnippetCharLimit),
			"message_id": msg.MessageID,
			"from_id":    msg.FromID,
			"created_at": msg.CreatedAt.UTC().Format(time.RFC3339),
		})
	}

	return toolResult{
		Status:  "ok",
		Summary: fmt.Sprintf("Found %d recent history snippets", len(matches)),
		Data: map[string]any{
			"query":    payload.Query,
			"scope":    map[string]any{"chat_id": callCtx.Scope.ChatID, "topic_id": callCtx.Scope.TopicID},
			"matches":  matches,
			"searched": len(snapshot.Messages),
		},
	}, nil
}
