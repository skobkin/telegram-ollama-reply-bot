package extractor

import (
	"context"
	"log/slog"
	"telegram-ollama-reply-bot/internal/logging"

	"github.com/getsentry/sentry-go"
	"github.com/go-shiori/go-readability"
)

type ReadabilityExtractor struct {
	logger *slog.Logger
}

func NewReadabilityExtractor(logger *slog.Logger) *ReadabilityExtractor {
	return &ReadabilityExtractor{logger: logger}
}

func (e *ReadabilityExtractor) GetArticleFromURL(ctx context.Context, url string) (Article, error) {
	logger := logging.FromContext(ctx, e.logger)
	logger.Info("extracting article", "url", url)

	article, err := readability.FromURL(url, ExtractionTimeout)
	if err != nil {
		logger.Warn("extraction failed", "url", url, "error", err)
		sentry.CaptureException(err)

		return Article{}, ErrExtractFailed
	}

	logger.Debug("article extracted", "url", url, "text_length", len(article.TextContent))

	return Article{
		Title: article.Title,
		Text:  article.TextContent,
		URL:   url,
	}, nil
}
