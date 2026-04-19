package tooluse

import (
	"context"
	"encoding/json"
)

func reminderStubHandler(toolName string) Handler {
	return func(_ context.Context, callCtx CallContext, args json.RawMessage) (toolResult, error) {
		return toolResult{
			Status:  "not_implemented",
			Summary: toolName + " is not implemented yet",
			Data: map[string]any{
				"chat_id":  callCtx.Scope.ChatID,
				"topic_id": callCtx.Scope.TopicID,
				"request":  args,
				"note":     "Future scheduled reminders must be delivered back to the original topic when topic_id is present.",
			},
		}, nil
	}
}
