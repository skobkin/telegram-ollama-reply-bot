package extractor

import (
	"context"
	"errors"
	"log/slog"
	"telegram-ollama-reply-bot/internal/logging"
	"time"

	"github.com/getsentry/sentry-go"
)

const (
	ExtractionTimeout = 10 * time.Second
)

var (
	ErrExtractFailed = errors.New("extraction failed")
)

type Article struct {
	Title string
	Text  string
	URL   string
}

type Extractor interface {
	GetArticleFromURL(ctx context.Context, url string) (Article, error)
}

type MultiExtractor struct {
	primary  Extractor
	fallback Extractor
	logger   *slog.Logger
}

func NewMultiExtractor(logger *slog.Logger) *MultiExtractor {
	if logger == nil {
		logger = slog.Default().With("pkg", "content/extractor")
	}

	return &MultiExtractor{
		primary:  NewReadabilityExtractor(logger.With("backend", "readability")),
		fallback: NewGoOseExtractor(logger.With("backend", "goose")),
		logger:   logger,
	}
}

func (e *MultiExtractor) GetArticleFromURL(ctx context.Context, url string) (Article, error) {
	logger := e.logger
	if ctx != nil {
		logger = logging.FromContext(ctx, e.logger).With("backend", "multi")
	}

	logger.Info("extracting article", "url", url)

	article, err := e.primary.GetArticleFromURL(ctx, url)
	if err == nil && article.Text != "" {
		logger.Info("article extracted with primary backend", "text_length", len(article.Text))

		return article, nil
	} else if err != nil {
		logger.Warn("primary extractor failed", "url", url, "error", err)
		sentry.CaptureException(err)
	}

	logger.Info("trying fallback extractor", "url", url)
	article, err = e.fallback.GetArticleFromURL(ctx, url)
	if err == nil && article.Text != "" {
		logger.Info("article extracted with fallback backend", "text_length", len(article.Text))

		return article, nil
	} else if err != nil {
		logger.Warn("fallback extractor failed", "url", url, "error", err)
		sentry.CaptureException(err)
	}

	logger.Error("all extractors failed", "url", url)

	return Article{}, ErrExtractFailed
}

func NewExtractor(logger *slog.Logger) Extractor {
	return NewMultiExtractor(logger)
}
