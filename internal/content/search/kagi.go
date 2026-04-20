package search

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"telegram-ollama-reply-bot/internal/provider"
)

const defaultKagiSearchURL = "https://kagi.com/api/v0/search"

type KagiSearcher struct {
	client    *http.Client
	apiKey    string
	searchURL string
	logger    *slog.Logger
}

func NewKagiSearcher(client *http.Client, apiKey string, logger *slog.Logger) *KagiSearcher {
	if logger == nil {
		logger = slog.Default()
	}

	return &KagiSearcher{
		client:    client,
		apiKey:    apiKey,
		searchURL: defaultKagiSearchURL,
		logger:    logger,
	}
}

func (s *KagiSearcher) Search(ctx context.Context, req Request) (Result, error) {
	queryURL, err := url.Parse(s.searchURL)
	if err != nil {
		return Result{}, provider.NewError("kagi", provider.ErrorKindBadRequest)
	}
	values := queryURL.Query()
	values.Set("q", req.Query)
	values.Set("limit", strconv.Itoa(normalizeMaxResults(req.MaxResults)))
	queryURL.RawQuery = values.Encode()

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, queryURL.String(), nil)
	if err != nil {
		return Result{}, provider.NewError("kagi", provider.ErrorKindBadRequest)
	}
	httpReq.Header.Set("Authorization", "Bot "+s.apiKey)

	resp, err := s.client.Do(httpReq)
	if err != nil {
		return Result{}, provider.NewError("kagi", provider.ErrorKindUpstreamUnavailable)
	}
	defer func() { _ = resp.Body.Close() }()

	rawBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Result{}, provider.NewError("kagi", provider.ErrorKindUpstreamUnavailable)
	}

	var payload struct {
		Data []struct {
			Type      int    `json:"t"`
			URL       string `json:"url"`
			Title     string `json:"title"`
			Snippet   string `json:"snippet"`
			Published string `json:"published"`
		} `json:"data"`
		Error []struct {
			Code int    `json:"code"`
			Msg  string `json:"msg"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rawBody, &payload); err != nil {
		return Result{}, provider.NewError("kagi", provider.ErrorKindUpstreamUnavailable)
	}

	if resp.StatusCode >= 400 || len(payload.Error) > 0 {
		return Result{}, classifyKagiError(resp.StatusCode, payload.Error, rawBody)
	}

	items := make([]ResultItem, 0, len(payload.Data))
	for _, item := range payload.Data {
		if item.Type != 0 {
			continue
		}

		items = append(items, ResultItem{
			Title:       item.Title,
			URL:         item.URL,
			Snippet:     normalizeWhitespace(item.Snippet),
			PublishedAt: item.Published,
		})
	}

	return Result{
		Provider: "kagi",
		Query:    req.Query,
		Results:  items,
	}, nil
}

func classifyKagiError(status int, errs []struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
}, body []byte) error {
	if len(errs) > 0 {
		switch errs[0].Code {
		case 2:
			return provider.NewError("kagi", provider.ErrorKindAuthFailed)
		case 100, 101:
			return provider.NewError("kagi", provider.ErrorKindCreditsExhausted)
		case 1:
			return provider.NewError("kagi", provider.ErrorKindBadRequest)
		}
	}

	text := strings.ToLower(string(body))
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden || strings.Contains(text, "unauthorized"):
		return provider.NewError("kagi", provider.ErrorKindAuthFailed)
	case status == http.StatusPaymentRequired || strings.Contains(text, "insufficient credit") || strings.Contains(text, "billing"):
		return provider.NewError("kagi", provider.ErrorKindCreditsExhausted)
	case status == http.StatusTooManyRequests:
		return provider.NewError("kagi", provider.ErrorKindRateLimited)
	case status >= 400 && status < 500:
		return provider.NewError("kagi", provider.ErrorKindBadRequest)
	default:
		return provider.NewError("kagi", provider.ErrorKindUpstreamUnavailable)
	}
}
