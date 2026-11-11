package extractor

import (
	"bytes"
	"context"
	"log/slog"
	nurl "net/url"

	"telegram-ollama-reply-bot/internal/content/fetch"
	"telegram-ollama-reply-bot/internal/logging"

	"github.com/getsentry/sentry-go"
	readability "github.com/markusmobius/go-readabilityV2"
)

// ReadabilityExtractor extracts article content with go-readabilityV2, the
// core-only fork of the Readeck readability implementation (the same
// mozilla/readability lineage as before). Fetching happens upstream in the
// shared policy fetcher; each call parses its own copy of the page, so
// concurrent extractions are safe.
type ReadabilityExtractor struct {
	logger *slog.Logger
}

func NewReadabilityExtractor(logger *slog.Logger) *ReadabilityExtractor {
	return &ReadabilityExtractor{logger: logger}
}

func (e *ReadabilityExtractor) Name() string {
	return "readability"
}

func (e *ReadabilityExtractor) Extract(ctx context.Context, page *fetch.Page) (Content, error) {
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

		return Content{}, ErrExtractFailed
	}

	var text bytes.Buffer
	if err := article.RenderText(&text); err != nil {
		logger.Warn("extracted article text rendering failed", "url", page.URL, "error", err)
		sentry.CaptureException(err)

		return Content{}, ErrExtractFailed
	}

	var html bytes.Buffer
	if err := article.RenderHTML(&html); err != nil {
		// Text is still usable; structure is optional for this backend.
		logger.Debug("extracted article html rendering failed", "url", page.URL, "error", err)
	}

	logger.Debug("article extracted", "url", page.URL, "text_length", len(text.String()))

	return Content{
		Title: article.Title(),
		Text:  text.String(),
		HTML:  html.String(),
	}, nil
}
