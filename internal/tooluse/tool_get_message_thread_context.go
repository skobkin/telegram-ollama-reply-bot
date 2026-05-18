package tooluse

import (
	"context"
	"encoding/json"
	"slices"
	"time"

	"telegram-ollama-reply-bot/internal/state"
)

const (
	messageThreadChainLimit         = 8
	messageThreadAdjacentPerNodeMax = 3
	messageThreadAdjacentTotalMax   = 12
)

func (r *Runtime) getMessageThreadContext(_ context.Context, callCtx CallContext, _ json.RawMessage) (toolResult, error) {
	snapshot := r.history.Snapshot(callCtx.Scope)
	if isEmptyThreadMessage(callCtx.Requester) {
		result := toolResult{
			Status:  "empty",
			Summary: "No current message context is available for thread lookup",
			Data: map[string]any{
				"chat_id":  callCtx.Scope.ChatID,
				"topic_id": callCtx.Scope.TopicID,
			},
		}
		logToolResult(callCtx, result, "chain_count", 0, "adjacent_reply_groups_count", 0, "adjacent_replies_count", 0)

		return result, nil
	}

	index := buildThreadMessageIndex(snapshot.Messages)
	chain, anchorReason, chainTruncated := reconstructThreadChain(callCtx.Requester, index)
	adjacentReplies, adjacentTruncated := collectAdjacentReplies(snapshot.Messages, chain)

	totalAdjacentReplies := 0
	for _, group := range adjacentReplies {
		replies, _ := group["replies"].([]map[string]any)
		totalAdjacentReplies += len(replies)
	}

	result := toolResult{
		Status:  "ok",
		Summary: "Message thread context retrieved",
		Data: map[string]any{
			"chat_id":                callCtx.Scope.ChatID,
			"topic_id":               callCtx.Scope.TopicID,
			"anchor_reason":          anchorReason,
			"anchor_message_id":      threadAnchorMessageID(chain),
			"snapshot_message_count": len(snapshot.Messages),
			"chain":                  threadMessagesView(chain),
			"adjacent_replies":       adjacentReplies,
			"truncated": map[string]any{
				"chain_depth":      chainTruncated,
				"adjacent_replies": adjacentTruncated,
			},
		},
	}
	logToolResult(callCtx, result,
		"chain_count", len(chain),
		"adjacent_reply_groups_count", len(adjacentReplies),
		"adjacent_replies_count", totalAdjacentReplies,
		"snapshot_message_count", len(snapshot.Messages),
	)

	return result, nil
}

func buildThreadMessageIndex(messages []state.Message) map[int]state.Message {
	result := make(map[int]state.Message, len(messages))
	for _, msg := range messages {
		if msg.MessageID == 0 {
			continue
		}

		if existing, ok := result[msg.MessageID]; ok {
			result[msg.MessageID] = mergeThreadMessage(existing, msg)

			continue
		}

		result[msg.MessageID] = cloneThreadMessage(msg)
	}

	return result
}

func reconstructThreadChain(requester state.Message, index map[int]state.Message) ([]state.Message, string, bool) {
	anchorReason := "request_message"
	if requester.ReplyTo != nil {
		anchorReason = "request_reply_target"
	}

	current := enrichThreadMessage(requester, index)
	chainNewestFirst := make([]state.Message, 0, messageThreadChainLimit)
	seen := make(map[int]struct{}, messageThreadChainLimit)
	truncated := false

	for !isEmptyThreadMessage(current) {
		if current.MessageID != 0 {
			if _, ok := seen[current.MessageID]; ok {
				break
			}
			seen[current.MessageID] = struct{}{}
		}

		if len(chainNewestFirst) >= messageThreadChainLimit {
			truncated = true

			break
		}
		chainNewestFirst = append(chainNewestFirst, current)

		if current.ReplyTo == nil {
			break
		}

		current = enrichThreadMessage(*current.ReplyTo, index)
	}

	slices.Reverse(chainNewestFirst)

	return chainNewestFirst, anchorReason, truncated
}

func collectAdjacentReplies(snapshot []state.Message, chain []state.Message) ([]map[string]any, bool) {
	if len(chain) == 0 || len(snapshot) == 0 {
		return make([]map[string]any, 0), false
	}

	chainIDs := make(map[int]struct{}, len(chain))
	for _, msg := range chain {
		if msg.MessageID != 0 {
			chainIDs[msg.MessageID] = struct{}{}
		}
	}

	total := 0
	truncated := false
	result := make([]map[string]any, 0, len(chain))

	for _, parent := range chain {
		if parent.MessageID == 0 {
			continue
		}
		if total >= messageThreadAdjacentTotalMax {
			truncated = true

			break
		}

		children := make([]state.Message, 0, messageThreadAdjacentPerNodeMax)
		for _, candidate := range snapshot {
			if candidate.ReplyTo == nil || candidate.ReplyTo.MessageID != parent.MessageID {
				continue
			}
			if _, ok := chainIDs[candidate.MessageID]; ok {
				continue
			}
			if len(children) >= messageThreadAdjacentPerNodeMax || total >= messageThreadAdjacentTotalMax {
				truncated = true

				break
			}

			children = append(children, candidate)
			total++
		}

		if len(children) == 0 {
			continue
		}

		result = append(result, map[string]any{
			"parent_message_id": parent.MessageID,
			"replies":           threadMessagesView(children),
		})
	}

	return result, truncated
}

func enrichThreadMessage(msg state.Message, index map[int]state.Message) state.Message {
	if msg.MessageID != 0 {
		if indexed, ok := index[msg.MessageID]; ok {
			return mergeThreadMessage(indexed, msg)
		}
	}

	return cloneThreadMessage(msg)
}

func mergeThreadMessage(primary, secondary state.Message) state.Message {
	result := cloneThreadMessage(primary)

	if len(secondary.Text) > len(result.Text) {
		result.Text = secondary.Text
	}
	if result.Name == "" {
		result.Name = secondary.Name
	}
	if result.Username == "" {
		result.Username = secondary.Username
	}
	if result.Image == "" {
		result.Image = secondary.Image
	}
	if result.MessageID == 0 {
		result.MessageID = secondary.MessageID
	}
	if result.FromID == 0 {
		result.FromID = secondary.FromID
	}
	if result.ChatID == 0 {
		result.ChatID = secondary.ChatID
	}
	if result.TopicID == 0 {
		result.TopicID = secondary.TopicID
	}
	if result.CreatedAt.IsZero() {
		result.CreatedAt = secondary.CreatedAt
	}
	result.IsMe = result.IsMe || secondary.IsMe
	result.IsUserRequest = result.IsUserRequest || secondary.IsUserRequest
	result.HasImage = result.HasImage || secondary.HasImage

	if result.ImageMeta == nil && secondary.ImageMeta != nil {
		meta := *secondary.ImageMeta
		result.ImageMeta = &meta
	}

	switch {
	case result.ReplyTo == nil && secondary.ReplyTo != nil:
		reply := cloneThreadMessage(*secondary.ReplyTo)
		result.ReplyTo = &reply
	case result.ReplyTo != nil && secondary.ReplyTo != nil:
		merged := mergeThreadMessage(*result.ReplyTo, *secondary.ReplyTo)
		result.ReplyTo = &merged
	}

	return result
}

func cloneThreadMessage(msg state.Message) state.Message {
	cloned := msg
	if msg.ReplyTo != nil {
		reply := cloneThreadMessage(*msg.ReplyTo)
		cloned.ReplyTo = &reply
	}
	if msg.ImageMeta != nil {
		meta := *msg.ImageMeta
		cloned.ImageMeta = &meta
	}

	return cloned
}

func isEmptyThreadMessage(msg state.Message) bool {
	return msg.MessageID == 0 &&
		msg.FromID == 0 &&
		msg.Name == "" &&
		msg.Username == "" &&
		msg.Text == "" &&
		msg.ReplyTo == nil &&
		msg.CreatedAt.IsZero() &&
		!msg.HasImage
}

func threadAnchorMessageID(chain []state.Message) int {
	if len(chain) == 0 {
		return 0
	}
	if len(chain) >= 2 {
		return chain[len(chain)-2].MessageID
	}

	return chain[len(chain)-1].MessageID
}

func threadMessagesView(messages []state.Message) []map[string]any {
	result := make([]map[string]any, 0, len(messages))
	for _, msg := range messages {
		result = append(result, threadMessageView(msg))
	}

	return result
}

func threadMessageView(msg state.Message) map[string]any {
	return map[string]any{
		"message_id":          msg.MessageID,
		"reply_to_message_id": replyToMessageID(msg),
		"from_id":             msg.FromID,
		"name":                msg.Name,
		"username":            msg.Username,
		"created_at":          threadMessageTime(msg.CreatedAt),
		"is_me":               msg.IsMe,
		"is_user_request":     msg.IsUserRequest,
		"has_image":           msg.HasImage,
		"text_preview":        threadMessagePreview(msg),
	}
}

func replyToMessageID(msg state.Message) int {
	if msg.ReplyTo == nil {
		return 0
	}

	return msg.ReplyTo.MessageID
}

func threadMessageTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}

	return value.UTC().Format(time.RFC3339)
}

func threadMessagePreview(msg state.Message) string {
	text := normalizeWhitespace(msg.Text)
	switch {
	case msg.HasImage && msg.Image != "" && text != "":
		text = "[Image: " + normalizeWhitespace(msg.Image) + "] " + text
	case msg.HasImage && msg.Image != "":
		text = "[Image: " + normalizeWhitespace(msg.Image) + "]"
	case msg.HasImage && text != "":
		text = "[Image] " + text
	case msg.HasImage:
		text = "[Image]"
	}

	return truncateUTF8(text, messageThreadSnippetCharLimit)
}
