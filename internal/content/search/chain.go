package search

import (
	"context"
	"errors"
	"log/slog"
	"sync"

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

// ValidateAuth probes every chain member that implements AuthValidator,
// CONCURRENTLY: the members share the caller's deadline, so one hung provider
// must not consume the budget of the others — a sequential Tavily probe that
// times out would otherwise hand its successor an already-expired context,
// turning a definitive rejection (which aborts the boot) into a transient
// timeout (which only logs). Results are classified in member order, so log
// lines and joined errors stay deterministic. Transient failures are logged
// and skipped — one flaky provider must not block the boot when the chain
// can route around it. A definitive rejection (auth failed, credits
// exhausted) proves the configured key cannot work, so all of them are
// joined into the returned error and abort the boot.
func (s *ChainSearcher) ValidateAuth(ctx context.Context) error {
	verdicts := make([]error, len(s.searchers))

	var wg sync.WaitGroup
	for i, entry := range s.searchers {
		validator, ok := entry.searcher.(AuthValidator)
		if !ok {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			verdicts[i] = validator.ValidateAuth(ctx)
		}()
	}
	wg.Wait()

	var definitive []error
	for i, entry := range s.searchers {
		err := verdicts[i]
		switch {
		case err == nil:
		case provider.IsDefinitiveRejection(err):
			definitive = append(definitive, err)
		default:
			s.logger.Warn("search provider auth validation failed, continuing", "provider", entry.name, "error", err)
		}
	}

	return errors.Join(definitive...)
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
