package search

import "context"

type Request struct {
	Query      string
	MaxResults int
}

type ResultItem struct {
	Title       string  `json:"title"`
	URL         string  `json:"url"`
	Snippet     string  `json:"snippet,omitempty"`
	PublishedAt string  `json:"published_at,omitempty"`
	Score       float64 `json:"score,omitempty"`
}

type Result struct {
	Provider string       `json:"provider"`
	Query    string       `json:"query"`
	Results  []ResultItem `json:"results"`
}

type Searcher interface {
	Search(ctx context.Context, req Request) (Result, error)
}
