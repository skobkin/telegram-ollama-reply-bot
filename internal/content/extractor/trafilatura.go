package extractor

import (
	"bytes"
	"context"
	"log/slog"
	nurl "net/url"

	"telegram-ollama-reply-bot/internal/content/fetch"
	"telegram-ollama-reply-bot/internal/logging"

	"github.com/getsentry/sentry-go"
	trafilatura "github.com/markusmobius/go-trafilatura"
	xhtml "golang.org/x/net/html"
)

// TrafilaturaExtractor extracts article content with go-trafilatura, a Go
// port of the trafilatura Python library — the strongest open-source
// extractor in upstream quality benchmarks, with rich metadata handling.
// Its internal fallback engines are disabled: this pipeline is the fallback
// chain, and a built-in readability/domdistiller fallback would duplicate
// the sibling backends.
type TrafilaturaExtractor struct {
	logger *slog.Logger
}

func NewTrafilaturaExtractor(logger *slog.Logger) *TrafilaturaExtractor {
	return &TrafilaturaExtractor{logger: logger}
}

func (e *TrafilaturaExtractor) Name() string {
	return "trafilatura"
}

func (e *TrafilaturaExtractor) Extract(ctx context.Context, page *fetch.Page) (Content, error) {
	logger := logging.FromContext(ctx, e.logger)
	logger.Debug("extracting article from fetched page", "url", page.URL)

	pageURL, err := nurl.Parse(page.URL)
	if err != nil {
		pageURL = nil
	}

	result, err := trafilatura.Extract(bytes.NewReader(page.Body), trafilatura.Options{
		OriginalURL: pageURL,
		// Keep links so the Markdown rendering preserves them.
		IncludeLinks: true,
		// Comments are rarely article content and dilute summaries.
		ExcludeComments: true,
	})
	if err != nil {
		logger.Warn("extraction failed", "url", page.URL, "error", err)
		sentry.CaptureException(err)

		return Content{}, ErrExtractFailed
	}
	if result == nil || result.ContentNode == nil {
		logger.Debug("no content extracted", "url", page.URL)

		return Content{}, ErrExtractFailed
	}

	var html bytes.Buffer
	if err := xhtml.Render(&html, result.ContentNode); err != nil {
		// Text is still usable; structure is optional for this backend.
		logger.Debug("extracted article html rendering failed", "url", page.URL, "error", err)
	}

	logger.Debug("article extracted", "url", page.URL, "text_length", len(result.ContentText))

	return Content{
		Title: result.Metadata.Title,
		Text:  result.ContentText,
		HTML:  html.String(),
	}, nil
}
