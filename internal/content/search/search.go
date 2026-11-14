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

// AuthValidator is an optional startup credential probe implemented by
// searchers that can cheaply verify their configured API key. ValidateAuth
// performs a minimal real API call and returns the classified provider
// error: a definitive rejection (auth failed, credits exhausted) means the
// configured key cannot work, while any other error may be transient and
// must not block a boot on its own.
type AuthValidator interface {
	ValidateAuth(ctx context.Context) error
}
