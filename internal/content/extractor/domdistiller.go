package extractor

import (
	"bytes"
	"context"
	"log/slog"

	"telegram-ollama-reply-bot/internal/content/fetch"
	"telegram-ollama-reply-bot/internal/logging"

	"github.com/getsentry/sentry-go"
	domdistiller "github.com/markusmobius/go-domdistiller"
	xhtml "golang.org/x/net/html"
)

// DomDistillerExtractor extracts article content with go-domdistiller, a Go
// port of Chromium's DOM Distiller. It is stateless: each call parses its own
// copy of the page, so concurrent extractions are safe.
type DomDistillerExtractor struct {
	logger *slog.Logger
}

func NewDomDistillerExtractor(logger *slog.Logger) *DomDistillerExtractor {
	return &DomDistillerExtractor{logger: logger}
}

func (e *DomDistillerExtractor) Name() string {
	return "domdistiller"
}

func (e *DomDistillerExtractor) Extract(ctx context.Context, page *fetch.Page) (Content, error) {
	logger := logging.FromContext(ctx, e.logger)
	logger.Debug("extracting article from fetched page", "url", page.URL)

	// The page URL is metadata context for the distiller (image source
	// resolution, pagination heuristics). The distiller does not absolutize
	// text-link hrefs, so Content.HTML may carry relative links; Markdown
	// rendering resolves them against the final URL.
	result, err := domdistiller.ApplyForReader(bytes.NewReader(page.Body), &domdistiller.Options{
		OriginalURL: parsePageURL(page),
		// The pipeline never follows next-page links, and pagination
		// detection measurably slows small pages (#127).
		SkipPagination: true,
	})
	if err != nil {
		logger.Warn("extraction failed", "url", page.URL, "error", err)
		sentry.CaptureException(err)

		return Content{}, ErrExtractFailed
	}

	// Emptiness is gated on text: the library wraps even empty results in a
	// non-nil <div> node, so the structural check alone would pass them.
	// The nil guard is defense-in-depth only.
	if result == nil || result.Text == "" {
		logger.Debug("no content extracted", "url", page.URL)

		return Content{Empty: true}, nil
	}

	var html bytes.Buffer
	if err := xhtml.Render(&html, result.Node); err != nil {
		// Text is still usable; structure is optional for this backend.
		logger.Debug("extracted article html rendering failed", "url", page.URL, "error", err)
	}

	logger.Debug("article extracted", "url", page.URL, "text_length", len(result.Text))

	return Content{
		Title: result.Title,
		Text:  result.Text,
		HTML:  html.String(),
	}, nil
}
