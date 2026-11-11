package extractor

import (
	"bytes"
	"context"
	"log/slog"

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

	article, err := readability.FromReader(bytes.NewReader(page.Body), parsePageURL(page))
	if err != nil {
		logger.Warn("extraction failed", "url", page.URL, "error", err)
		sentry.CaptureException(err)

		return Content{}, ErrExtractFailed
	}

	// An unextractable page (the parser found no article) comes back with a
	// nil Node and nil error. That is a normal outcome, not a failure:
	// report it as empty and let the pipeline try the next backend.
	if article.Node == nil {
		logger.Debug("no content extracted", "url", page.URL)

		return Content{Empty: true}, nil
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
