package tooluse

import (
	"context"
	"encoding/json"
	"strings"

	"telegram-ollama-reply-bot/internal/content/search"
)

func (r *Runtime) searchWeb(ctx context.Context, callCtx CallContext, args json.RawMessage) (toolResult, error) {
	if r.searcher == nil {
		return toolResult{}, NewToolError("tool_unavailable", ErrorUnavailable, "Web search is unavailable.", false, "")
	}

	var payload struct {
		Query      string `json:"query"`
		MaxResults int    `json:"max_results"`
	}
	if err := json.Unmarshal(args, &payload); err != nil {
		return toolResult{}, invalidArguments("Provide a JSON object matching the tool schema.")
	}

	payload.Query = strings.TrimSpace(payload.Query)
	if payload.Query == "" {
		return toolResult{}, invalidArguments("query is required")
	}

	searchResult, err := r.searcher.Search(ctx, search.Request{
		Query:      payload.Query,
		MaxResults: payload.MaxResults,
	})
	if err != nil {
		return toolResult{}, err
	}

	result := toolResult{
		Status:  "ok",
		Summary: "Web search completed",
		Data:    searchResult,
	}
	logToolResult(callCtx, result, "results_count", len(searchResult.Results))

	return result, nil
}
