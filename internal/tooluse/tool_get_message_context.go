package tooluse

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"unicode/utf8"

	"telegram-ollama-reply-bot/internal/state"
)

const (
	messageContextMaxNeighbours     = 10
	messageContextDefaultNeighbours = 3
	messageContextNameRunes         = 64
	messageContextUsernameRunes     = 32
)

// getMessageContext returns one retained message with a bounded number of
// retained neighbours on each side. Neighbours are ordered by Telegram message
// ID (monotonic within a chat), independent of insertion order, and listed
// nearest first so budget reduction drops the farthest ones first.
func (r *Runtime) getMessageContext(_ context.Context, callCtx CallContext, args json.RawMessage) (toolResult, error) {
	var payload struct {
		MessageID int  `json:"message_id"`
		Before    *int `json:"before"`
		After     *int `json:"after"`
	}
	if !decodeToolJSON(args, &payload) || payload.MessageID < 1 {
		return toolResult{}, invalidArguments("Provide message_id (a positive integer) and optional before/after counts.")
	}
	before, after := messageContextDefaultNeighbours, messageContextDefaultNeighbours
	if payload.Before != nil {
		before = *payload.Before
	}
	if payload.After != nil {
		after = *payload.After
	}
	if before < 0 || before > messageContextMaxNeighbours || after < 0 || after > messageContextMaxNeighbours {
		return toolResult{}, invalidArguments(fmt.Sprintf("before and after must be between 0 and %d.", messageContextMaxNeighbours))
	}

	snapshot := r.history.Snapshot(callCtx.Scope)
	index := buildThreadMessageIndex(snapshot.Messages) // merges duplicates; skips ID 0
	ids := make([]int, 0, len(index))
	for id := range index {
		ids = append(ids, id)
	}
	slices.Sort(ids)

	position, found := slices.BinarySearch(ids, payload.MessageID)
	if !found {
		result := ErrorResult(NewToolError("message_not_in_history", ErrorNotFound,
			"The message is not in the retained history of this chat/topic.", false,
			"Only retained history of the current chat/topic is available; find IDs with search_history."))
		logToolResult(callCtx, result, "retained_messages", len(snapshot.Messages))

		return result, nil
	}

	beforeViews := make([]map[string]any, 0, before)
	for i := position - 1; i >= 0 && len(beforeViews) < before; i-- {
		beforeViews = append(beforeViews, messageContextView(index[ids[i]]))
	}
	afterViews := make([]map[string]any, 0, after)
	for i := position + 1; i < len(ids) && len(afterViews) < after; i++ {
		afterViews = append(afterViews, messageContextView(index[ids[i]]))
	}

	result := toolResult{
		Status:  "ok",
		Summary: "Message context retrieved",
		Data: map[string]any{
			"chat_id":   callCtx.Scope.ChatID,
			"topic_id":  callCtx.Scope.TopicID,
			"target":    messageContextView(index[payload.MessageID]),
			"before":    beforeViews,
			"after":     afterViews,
			"requested": map[string]any{"before": before, "after": after},
			// Retained-window boundaries only: the returned window includes the
			// oldest/newest retained message. They say nothing about messages
			// that may have existed and been discarded.
			"retained_window": map[string]any{
				"start_reached":          position-len(beforeViews) == 0,
				"end_reached":            position+len(afterViews) == len(ids)-1,
				"retained_messages":      len(snapshot.Messages),
				"earlier_summary_exists": snapshot.EarlierSummary != "",
			},
		},
	}
	logToolResult(callCtx, result, "before_count", len(beforeViews), "after_count", len(afterViews), "retained_messages", len(snapshot.Messages))

	return result, nil
}

// messageContextView is the thread view with author fields clamped, so even a
// maximum-length author with heavily escaped text stays deliverable. The
// *_truncated flags are set only when a value was actually shortened.
func messageContextView(msg state.Message) map[string]any {
	view := threadMessageView(msg)
	view["name"] = truncateUTF8(msg.Name, messageContextNameRunes)
	view["name_truncated"] = utf8.RuneCountInString(msg.Name) > messageContextNameRunes
	view["username"] = truncateUTF8(msg.Username, messageContextUsernameRunes)
	view["username_truncated"] = utf8.RuneCountInString(msg.Username) > messageContextUsernameRunes

	return view
}
