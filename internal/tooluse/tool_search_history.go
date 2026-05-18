package tooluse

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"telegram-ollama-reply-bot/internal/state"
)

func (r *Runtime) searchHistory(_ context.Context, callCtx CallContext, args json.RawMessage) (toolResult, error) {
	var payload struct {
		Keywords  []string `json:"keywords"`
		MatchMode string   `json:"match_mode"`
		Limit     int      `json:"limit"`
	}
	if !decodeToolJSON(args, &payload) && string(args) != "" && string(args) != "null" {
		return toolResult{}, fmt.Errorf("parse arguments: invalid search_history payload")
	}
	if len(payload.Keywords) == 0 {
		result := searchHistoryErrorResult("missing_required_field", "keywords must contain at least one search keyword")
		logToolResult(callCtx, result, "results_count", 0)

		return result, nil
	}
	matchMode := state.HistorySearchMatchMode(payload.MatchMode)
	if matchMode == "" {
		matchMode = state.HistorySearchMatchModeAll
	}
	if matchMode != state.HistorySearchMatchModeAll && matchMode != state.HistorySearchMatchModeAny {
		result := searchHistoryErrorResult("invalid_match_mode", "match_mode must be one of all, any")
		logToolResult(callCtx, result, "results_count", 0)

		return result, nil
	}
	if payload.Limit <= 0 {
		payload.Limit = defaultSearchHistoryResultLimit
	}
	if payload.Limit > maxSearchHistoryResultLimit {
		payload.Limit = maxSearchHistoryResultLimit
	}

	snapshot := r.history.Snapshot(callCtx.Scope)
	searchMatches := r.history.Search(callCtx.Scope, state.HistorySearchQuery{
		Keywords:  payload.Keywords,
		MatchMode: matchMode,
		Limit:     payload.Limit,
	})
	matches := make([]map[string]any, 0, len(searchMatches))
	for _, match := range searchMatches {
		msg := match.Message
		matches = append(matches, map[string]any{
			"name":             msg.Name,
			"username":         msg.Username,
			"text":             truncateUTF8(normalizeWhitespace(msg.Text), searchHistorySnippetCharLimit),
			"message_id":       msg.MessageID,
			"from_id":          msg.FromID,
			"created_at":       msg.CreatedAt.UTC().Format(time.RFC3339),
			"score":            match.Score,
			"match_kind":       match.MatchKind,
			"matched_keywords": match.MatchedKeywords,
		})
	}

	result := toolResult{
		Status:  "ok",
		Summary: fmt.Sprintf("Found %d history matches", len(matches)),
		Data: map[string]any{
			"keywords":   payload.Keywords,
			"match_mode": matchMode,
			"scope":      map[string]any{"chat_id": callCtx.Scope.ChatID, "topic_id": callCtx.Scope.TopicID},
			"matches":    matches,
			"searched":   len(snapshot.Messages),
		},
	}
	logToolResult(callCtx, result, "results_count", len(matches), "searched_count", len(snapshot.Messages))

	return result, nil
}

func searchHistoryErrorResult(code, message string) toolResult {
	return toolResult{
		Status: "error",
		Data: map[string]any{
			"error": toolErrorPayload{
				Code:    code,
				Message: message,
			},
		},
	}
}
