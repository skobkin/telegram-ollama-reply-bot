package tooluse

import (
	"context"
	"encoding/json"
	"time"

	"telegram-ollama-reply-bot/internal/state"
)

func (r *Runtime) getHistoryBounds(_ context.Context, callCtx CallContext, _ json.RawMessage) (toolResult, error) {
	snapshot := r.history.Snapshot(callCtx.Scope)
	if len(snapshot.Messages) == 0 {
		return toolResult{
			Status:  "empty",
			Summary: "No in-memory history is available for the current chat/topic",
			Data: map[string]any{
				"chat_id":  callCtx.Scope.ChatID,
				"topic_id": callCtx.Scope.TopicID,
			},
		}, nil
	}

	recent := snapshot.Messages
	if limit := r.config.RecentHistoryLimit; limit > 0 && len(recent) > limit {
		recent = recent[len(recent)-limit:]
	}

	return toolResult{
		Status:  "ok",
		Summary: "History bounds retrieved",
		Data: map[string]any{
			"chat_id":        callCtx.Scope.ChatID,
			"topic_id":       callCtx.Scope.TopicID,
			"full_history":   historyBoundsView(snapshot.Messages),
			"recent_history": historyBoundsView(recent),
		},
	}, nil
}

func historyBoundsView(messages []state.Message) map[string]any {
	first := messages[0].CreatedAt.UTC()
	last := messages[len(messages)-1].CreatedAt.UTC()

	return map[string]any{
		"oldest_message_at": first.Format(time.RFC3339),
		"newest_message_at": last.Format(time.RFC3339),
		"message_count":     len(messages),
		"span_seconds":      int64(last.Sub(first).Seconds()),
	}
}
