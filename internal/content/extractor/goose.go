package extractor

import (
	"context"
	"log/slog"
	"telegram-ollama-reply-bot/internal/content/fetch"
	"telegram-ollama-reply-bot/internal/logging"

	goose "github.com/advancedlogic/GoOse/pkg/goose"
	"github.com/getsentry/sentry-go"
)

// GoOseExtractor is the fallback extraction backend. It runs synchronously
// over the already-fetched page: no internal HTTP client, no goroutine race,
// and therefore no abandoned goroutines on cancellation — cancellation is
// handled upstream by the shared fetcher.
type GoOseExtractor struct {
	goose  goose.Goose
	logger *slog.Logger
}

func NewGoOseExtractor(logger *slog.Logger) *GoOseExtractor {
	return &GoOseExtractor{
		goose:  goose.New(),
		logger: logger,
	}
}

func (e *GoOseExtractor) name() string {
	return "goose"
}

func (e *GoOseExtractor) extractPage(ctx context.Context, page *fetch.Page) (Article, error) {
	logger := logging.FromContext(ctx, e.logger)
	logger.Debug("extracting article from fetched page", "url", page.URL)

	article, err := e.goose.ExtractFromRawHTML(string(page.Body), page.URL)
	if err != nil {
		logger.Warn("extraction failed", "url", page.URL, "error", err)
		sentry.CaptureException(err)

		return Article{}, ErrExtractFailed
	}

	logger.Debug("article extracted", "url", page.URL, "text_length", len(article.CleanedText))

	return Article{
		Title: article.Title,
		Text:  article.CleanedText,
		URL:   article.FinalURL,
	}, nil
}
