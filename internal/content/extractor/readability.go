package extractor

import (
	"bytes"
	"context"
	"log/slog"
	"telegram-ollama-reply-bot/internal/logging"

	readability "codeberg.org/readeck/go-readability/v2"
	"github.com/getsentry/sentry-go"
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

	var text bytes.Buffer
	if err := article.RenderText(&text); err != nil {
		logger.Warn("extracted article text rendering failed", "url", url, "error", err)
		sentry.CaptureException(err)

		return Article{}, ErrExtractFailed
	}

	logger.Debug("article extracted", "url", url, "text_length", len(text.String()))

	return Article{
		Title: article.Title(),
		Text:  text.String(),
		URL:   url,
	}, nil
}
