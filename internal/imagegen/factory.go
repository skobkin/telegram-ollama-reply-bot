package imagegen

import (
	"fmt"
	"log/slog"

	"telegram-ollama-reply-bot/internal/config"
)

// NewFromConfig builds the Service for the configured backend. It returns
// (nil, nil) when image generation is disabled (empty backend), so callers
// attach the generator only when one exists. client carries every backend
// request; production passes the secret-scrubbing transport.
//
// This switch is the single registration point for backends: a new backend
// adds a file implementing Backend, a config subsection validated in
// config.ImageGenerationConfig, a case here, and its credentials in the app's
// secret scrubber.
func NewFromConfig(cfg config.ImageGenerationConfig, client Doer, logger *slog.Logger) (*Service, error) {
	if logger == nil {
		logger = slog.Default()
	}

	var backend Backend
	switch cfg.Backend {
	case "":
		return nil, nil //nolint:nilnil // disabled feature: no service and no error
	case config.ImageGenerationBackendOpenAICompat:
		openAI, err := NewOpenAICompat(
			cfg.OpenAICompat.BaseURL,
			cfg.OpenAICompat.APIToken,
			client,
			cfg.MaxImageBytes,
			logger.With("backend", config.ImageGenerationBackendOpenAICompat),
		)
		if err != nil {
			return nil, err
		}
		backend = openAI
	default:
		return nil, fmt.Errorf("%w: unknown backend %q", ErrMisconfigured, cfg.Backend)
	}

	return NewService(backend, Options{
		Timeout:       cfg.Timeout,
		MaxImageBytes: cfg.MaxImageBytes,
		MaxConcurrent: cfg.MaxConcurrent,
	}, logger), nil
}
