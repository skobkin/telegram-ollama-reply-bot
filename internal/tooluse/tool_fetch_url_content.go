package tooluse

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

func (r *Runtime) fetchURLContent(ctx context.Context, callCtx CallContext, args json.RawMessage) (toolResult, error) {
	var payload struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(args, &payload); err != nil {
		return toolResult{}, fmt.Errorf("parse arguments: %w", err)
	}
	if !isValidToolURL(payload.URL) {
		return toolResult{}, fmt.Errorf("url must use http or https")
	}

	article, err := r.extractor.GetArticleFromURL(ctx, payload.URL)
	if err != nil {
		return toolResult{}, err
	}

	// The text field carries the Markdown rendering when available: it
	// preserves article structure within the same budget and field name the
	// model already knows. Plain text is the fallback.
	content := article.Markdown
	if content == "" {
		content = article.Text
	}
	content = strings.TrimSpace(content)

	result := toolResult{
		Status:  "ok",
		Summary: "URL content fetched",
		Data: map[string]any{
			"chat_id":     callCtx.Scope.ChatID,
			"topic_id":    callCtx.Scope.TopicID,
			"url":         article.URL,
			"title":       article.Title,
			"text":        truncateUTF8(content, fetchURLContentTextCharLimit),
			"extractedAt": time.Now().UTC().Format(time.RFC3339),
		},
	}
	logToolResult(callCtx, result,
		"url", article.URL,
		"title", article.Title,
		"text_length", len(content),
	)

	return result, nil
}
