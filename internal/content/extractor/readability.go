package extractor

import (
	"bytes"
	"context"
	"log/slog"
	nurl "net/url"
	"telegram-ollama-reply-bot/internal/content/fetch"
	"telegram-ollama-reply-bot/internal/logging"

	readability "codeberg.org/readeck/go-readability/v2"
	"github.com/getsentry/sentry-go"
)

// ReadabilityExtractor is the primary extraction backend. It parses the
// fetched page with the Readeck readability implementation (the fetching
// happens upstream, in the shared policy fetcher).
type ReadabilityExtractor struct {
	logger *slog.Logger
}

func NewReadabilityExtractor(logger *slog.Logger) *ReadabilityExtractor {
	return &ReadabilityExtractor{logger: logger}
}

func (e *ReadabilityExtractor) name() string {
	return "readability"
}

func (e *ReadabilityExtractor) extractPage(ctx context.Context, page *fetch.Page) (Article, error) {
	logger := logging.FromContext(ctx, e.logger)
	logger.Debug("extracting article from fetched page", "url", page.URL)

	// The page URL is metadata context for the parser (link resolution,
	// canonical URLs); a page whose final URL does not parse is still worth
	// extracting, just without that context.
	pageURL, err := nurl.Parse(page.URL)
	if err != nil {
		pageURL = nil
	}

	article, err := readability.FromReader(bytes.NewReader(page.Body), pageURL)
	if err != nil {
		logger.Warn("extraction failed", "url", page.URL, "error", err)
		sentry.CaptureException(err)

		return Article{}, ErrExtractFailed
	}

	var text bytes.Buffer
	if err := article.RenderText(&text); err != nil {
		logger.Warn("extracted article text rendering failed", "url", page.URL, "error", err)
		sentry.CaptureException(err)

		return Article{}, ErrExtractFailed
	}

	logger.Debug("article extracted", "url", page.URL, "text_length", len(text.String()))

	return Article{
		Title: article.Title(),
		Text:  text.String(),
		URL:   page.URL,
	}, nil
}
