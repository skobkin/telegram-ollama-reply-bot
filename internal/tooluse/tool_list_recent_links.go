package tooluse

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

func (r *Runtime) listRecentLinks(_ context.Context, callCtx CallContext, args json.RawMessage) (toolResult, error) {
	var payload struct {
		Limit int `json:"limit"`
	}
	if err := json.Unmarshal(args, &payload); err != nil && string(args) != "" && string(args) != "null" {
		return toolResult{}, fmt.Errorf("parse arguments: %w", err)
	}
	if payload.Limit <= 0 {
		payload.Limit = defaultRecentLinksResultLimit
	}
	if payload.Limit > maxRecentLinksResultLimit {
		payload.Limit = maxRecentLinksResultLimit
	}

	snapshot := r.history.Snapshot(callCtx.Scope)
	seen := make(map[string]struct{}, payload.Limit)
	links := make([]map[string]any, 0, payload.Limit)

	for i := len(snapshot.Messages) - 1; i >= 0 && len(links) < payload.Limit; i-- {
		msg := snapshot.Messages[i]
		urls := extractHTTPURLs(msg.Text)
		if len(urls) == 0 {
			continue
		}

		for _, link := range urls {
			if _, ok := seen[link]; ok {
				continue
			}
			seen[link] = struct{}{}

			links = append(links, map[string]any{
				"url":          link,
				"name":         msg.Name,
				"username":     msg.Username,
				"message_id":   msg.MessageID,
				"from_id":      msg.FromID,
				"created_at":   msg.CreatedAt.UTC().Format(time.RFC3339),
				"text_snippet": truncateUTF8(normalizeWhitespace(msg.Text), recentLinksSnippetCharLimit),
			})
			if len(links) >= payload.Limit {
				break
			}
		}
	}

	if len(links) == 0 {
		result := toolResult{
			Status:  "empty",
			Summary: "No recent HTTP or HTTPS links were found in the current chat/topic history",
			Data: map[string]any{
				"chat_id":  callCtx.Scope.ChatID,
				"topic_id": callCtx.Scope.TopicID,
			},
		}
		logToolResult(callCtx, result, "links_count", 0)

		return result, nil
	}

	result := toolResult{
		Status:  "ok",
		Summary: fmt.Sprintf("Found %d recent unique link(s)", len(links)),
		Data: map[string]any{
			"chat_id":  callCtx.Scope.ChatID,
			"topic_id": callCtx.Scope.TopicID,
			"links":    links,
		},
	}
	logToolResult(callCtx, result, "links_count", len(links))

	return result, nil
}
