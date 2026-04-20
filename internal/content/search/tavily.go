package search

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"telegram-ollama-reply-bot/internal/provider"
)

var tavilySearchURL = "https://api.tavily.com/search"

type TavilySearcher struct {
	client *http.Client
	apiKey string
	logger *slog.Logger
}

func NewTavilySearcher(client *http.Client, apiKey string, logger *slog.Logger) *TavilySearcher {
	if logger == nil {
		logger = slog.Default()
	}

	return &TavilySearcher{client: client, apiKey: apiKey, logger: logger}
}

func (s *TavilySearcher) Search(ctx context.Context, req Request) (Result, error) {
	payload := map[string]any{
		"query":        req.Query,
		"max_results":  normalizeMaxResults(req.MaxResults),
		"search_depth": "basic",
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return Result{}, provider.NewError("tavily", provider.ErrorKindBadRequest)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, tavilySearchURL, bytes.NewReader(body))
	if err != nil {
		return Result{}, provider.NewError("tavily", provider.ErrorKindBadRequest)
	}
	httpReq.Header.Set("Authorization", "Bearer "+s.apiKey)
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := s.client.Do(httpReq)
	if err != nil {
		return Result{}, provider.NewError("tavily", provider.ErrorKindUpstreamUnavailable)
	}
	defer func() { _ = resp.Body.Close() }()

	rawBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Result{}, provider.NewError("tavily", provider.ErrorKindUpstreamUnavailable)
	}

	if resp.StatusCode >= 400 {
		return Result{}, classifyTavilyError(resp.StatusCode, rawBody)
	}

	var payloadResp struct {
		Query   string `json:"query"`
		Results []struct {
			Title         string  `json:"title"`
			URL           string  `json:"url"`
			Content       string  `json:"content"`
			Score         float64 `json:"score"`
			PublishedDate string  `json:"published_date"`
		} `json:"results"`
	}
	if err := json.Unmarshal(rawBody, &payloadResp); err != nil {
		return Result{}, provider.NewError("tavily", provider.ErrorKindUpstreamUnavailable)
	}

	items := make([]ResultItem, 0, len(payloadResp.Results))
	for _, item := range payloadResp.Results {
		items = append(items, ResultItem{
			Title:       item.Title,
			URL:         item.URL,
			Snippet:     normalizeWhitespace(item.Content),
			PublishedAt: item.PublishedDate,
			Score:       item.Score,
		})
	}

	return Result{
		Provider: "tavily",
		Query:    payloadResp.Query,
		Results:  items,
	}, nil
}

func classifyTavilyError(status int, body []byte) error {
	var payload struct {
		Error   string `json:"error"`
		Message string `json:"message"`
		Detail  string `json:"detail"`
	}
	_ = json.Unmarshal(body, &payload)

	text := strings.ToLower(strings.Join([]string{
		http.StatusText(status),
		payload.Error,
		payload.Message,
		payload.Detail,
		string(body),
	}, " "))

	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden || strings.Contains(text, "unauthorized") || strings.Contains(text, "invalid api key"):
		return provider.NewError("tavily", provider.ErrorKindAuthFailed)
	case status == http.StatusPaymentRequired || strings.Contains(text, "credit") || strings.Contains(text, "quota") || strings.Contains(text, "insufficient balance"):
		return provider.NewError("tavily", provider.ErrorKindCreditsExhausted)
	case status == http.StatusTooManyRequests:
		return provider.NewError("tavily", provider.ErrorKindRateLimited)
	case status >= 400 && status < 500:
		return provider.NewError("tavily", provider.ErrorKindBadRequest)
	default:
		return provider.NewError("tavily", provider.ErrorKindUpstreamUnavailable)
	}
}

func normalizeMaxResults(value int) int {
	if value <= 0 {
		return 5
	}
	if value > 10 {
		return 10
	}

	return value
}

func normalizeWhitespace(text string) string {
	return strings.Join(strings.Fields(text), " ")
}
