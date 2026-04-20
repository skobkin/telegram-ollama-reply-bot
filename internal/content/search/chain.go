package search

import (
	"context"
	"log/slog"

	"telegram-ollama-reply-bot/internal/provider"
)

type namedSearcher struct {
	name     string
	searcher Searcher
}

type ChainSearcher struct {
	searchers []namedSearcher
	logger    *slog.Logger
}

func (s *ChainSearcher) Search(ctx context.Context, req Request) (Result, error) {
	failures := make(map[string]error, len(s.searchers))
	order := make([]string, 0, len(s.searchers))

	for _, searcher := range s.searchers {
		order = append(order, searcher.name)

		result, err := searcher.searcher.Search(ctx, req)
		if err == nil {
			return result, nil
		}

		failures[searcher.name] = err
	}

	return Result{}, &provider.ChainError{Attempts: provider.AttemptsFromErrors(failures, order)}
}
