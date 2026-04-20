package tooluse

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"telegram-ollama-reply-bot/internal/content/search"
)

func (r *Runtime) searchWeb(ctx context.Context, _ CallContext, args json.RawMessage) (toolResult, error) {
	if r.searcher == nil {
		return toolResult{}, fmt.Errorf("web search is unavailable")
	}

	var payload struct {
		Query      string `json:"query"`
		MaxResults int    `json:"max_results"`
	}
	if err := json.Unmarshal(args, &payload); err != nil {
		return toolResult{}, fmt.Errorf("parse arguments: %w", err)
	}

	payload.Query = strings.TrimSpace(payload.Query)
	if payload.Query == "" {
		return toolResult{}, fmt.Errorf("query is required")
	}

	result, err := r.searcher.Search(ctx, search.Request{
		Query:      payload.Query,
		MaxResults: payload.MaxResults,
	})
	if err != nil {
		return toolResult{}, err
	}

	return toolResult{
		Status:  "ok",
		Summary: "Web search completed",
		Data:    result,
	}, nil
}
