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
		return searchHistoryErrorResult("missing_required_field", "keywords must contain at least one search keyword"), nil
	}
	matchMode := state.HistorySearchMatchMode(payload.MatchMode)
	if matchMode == "" {
		matchMode = state.HistorySearchMatchModeAll
	}
	if matchMode != state.HistorySearchMatchModeAll && matchMode != state.HistorySearchMatchModeAny {
		return searchHistoryErrorResult("invalid_match_mode", "match_mode must be one of all, any"), nil
	}
	if payload.Limit <= 0 {
		payload.Limit = defaultRecentHistoryResultLimit
	}
	if payload.Limit > maxRecentHistoryResultLimit {
		payload.Limit = maxRecentHistoryResultLimit
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
			"text":             truncateUTF8(normalizeWhitespace(msg.Text), searchRecentHistorySnippetCharLimit),
			"message_id":       msg.MessageID,
			"from_id":          msg.FromID,
			"created_at":       msg.CreatedAt.UTC().Format(time.RFC3339),
			"score":            match.Score,
			"match_kind":       match.MatchKind,
			"matched_keywords": match.MatchedKeywords,
		})
	}

	return toolResult{
		Status:  "ok",
		Summary: fmt.Sprintf("Found %d history matches", len(matches)),
		Data: map[string]any{
			"keywords":   payload.Keywords,
			"match_mode": matchMode,
			"scope":      map[string]any{"chat_id": callCtx.Scope.ChatID, "topic_id": callCtx.Scope.TopicID},
			"matches":    matches,
			"searched":   len(snapshot.Messages),
		},
	}, nil
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
