package search

import (
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"telegram-ollama-reply-bot/internal/config"
	"telegram-ollama-reply-bot/internal/provider"
)

func NewFromConfig(cfg config.Config, client *http.Client, logger *slog.Logger) (Searcher, error) {
	if logger == nil {
		logger = slog.Default()
	}
	if client == nil {
		return nil, fmt.Errorf("search http client is required")
	}

	switch strings.TrimSpace(cfg.Search.Backend) {
	case "", config.SearchBackendNone:
		return nil, nil
	case config.SearchBackendTavily:
		if strings.TrimSpace(cfg.Providers.Tavily.APIKey) == "" {
			return nil, provider.NewError(config.SearchBackendTavily, provider.ErrorKindMisconfigured)
		}

		return NewTavilySearcher(client, cfg.Providers.Tavily.APIKey, logger.With("provider", config.SearchBackendTavily)), nil
	case config.SearchBackendKagi:
		if strings.TrimSpace(cfg.Providers.Kagi.APIKey) == "" {
			return nil, provider.NewError(config.SearchBackendKagi, provider.ErrorKindMisconfigured)
		}

		return NewKagiSearcher(client, cfg.Providers.Kagi.APIKey, logger.With("provider", config.SearchBackendKagi)), nil
	case config.SearchBackendChain:
		return newChainSearcher(cfg, client, logger)
	default:
		return nil, fmt.Errorf("unknown search backend %q", cfg.Search.Backend)
	}
}

func newChainSearcher(cfg config.Config, client *http.Client, logger *slog.Logger) (Searcher, error) {
	if len(cfg.Search.Chain) == 0 {
		return nil, provider.NewError(config.SearchBackendChain, provider.ErrorKindMisconfigured)
	}

	searchers := make([]namedSearcher, 0, len(cfg.Search.Chain))
	for _, backend := range cfg.Search.Chain {
		switch backend {
		case config.SearchBackendTavily:
			if strings.TrimSpace(cfg.Providers.Tavily.APIKey) == "" {
				return nil, provider.NewError(config.SearchBackendTavily, provider.ErrorKindMisconfigured)
			}

			searchers = append(searchers, namedSearcher{
				name:     backend,
				searcher: NewTavilySearcher(client, cfg.Providers.Tavily.APIKey, logger.With("provider", backend)),
			})
		case config.SearchBackendKagi:
			if strings.TrimSpace(cfg.Providers.Kagi.APIKey) == "" {
				return nil, provider.NewError(config.SearchBackendKagi, provider.ErrorKindMisconfigured)
			}

			searchers = append(searchers, namedSearcher{
				name:     backend,
				searcher: NewKagiSearcher(client, cfg.Providers.Kagi.APIKey, logger.With("provider", backend)),
			})
		default:
			return nil, fmt.Errorf("unsupported search backend %q in chain", backend)
		}
	}

	return &ChainSearcher{
		searchers: searchers,
		logger:    logger.With("provider", config.SearchBackendChain),
	}, nil
}
