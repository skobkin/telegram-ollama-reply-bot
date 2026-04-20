package tooluse

import (
	"context"
	"encoding/json"
	"strings"
)

func (r *Runtime) getConversationSummary(_ context.Context, callCtx CallContext, _ json.RawMessage) (toolResult, error) {
	snapshot := r.history.Snapshot(callCtx.Scope)
	if strings.TrimSpace(snapshot.EarlierSummary) == "" {
		result := toolResult{
			Status:  "empty",
			Summary: "No in-memory conversation summary is available for the current chat/topic",
			Data: map[string]any{
				"chat_id":  callCtx.Scope.ChatID,
				"topic_id": callCtx.Scope.TopicID,
			},
		}
		logToolResult(callCtx, result, "summary_length", 0)

		return result, nil
	}

	result := toolResult{
		Status:  "ok",
		Summary: "Conversation summary retrieved",
		Data: map[string]any{
			"chat_id":  callCtx.Scope.ChatID,
			"topic_id": callCtx.Scope.TopicID,
			"summary":  truncateUTF8(snapshot.EarlierSummary, conversationSummaryTextCharLimit),
		},
	}
	logToolResult(callCtx, result, "summary_length", len(strings.TrimSpace(snapshot.EarlierSummary)))

	return result, nil
}
