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

	result := toolResult{
		Status:  "ok",
		Summary: "URL content fetched",
		Data: map[string]any{
			"chat_id":     callCtx.Scope.ChatID,
			"topic_id":    callCtx.Scope.TopicID,
			"url":         article.URL,
			"title":       article.Title,
			"text":        truncateUTF8(strings.TrimSpace(article.Text), fetchURLContentTextCharLimit),
			"extractedAt": time.Now().UTC().Format(time.RFC3339),
		},
	}
	logToolResult(callCtx, result,
		"url", article.URL,
		"title", article.Title,
		"text_length", len(strings.TrimSpace(article.Text)),
	)

	return result, nil
}
