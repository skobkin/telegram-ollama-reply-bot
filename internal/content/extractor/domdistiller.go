package extractor

import (
	"bytes"
	"context"
	"log/slog"
	nurl "net/url"

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

	// The page URL is metadata context for the distiller (absolute-link
	// rewriting, pagination heuristics); a page whose final URL does not
	// parse is still worth extracting, just without that context.
	pageURL, err := nurl.Parse(page.URL)
	if err != nil {
		pageURL = nil
	}

	result, err := domdistiller.ApplyForReader(bytes.NewReader(page.Body), &domdistiller.Options{
		OriginalURL: pageURL,
		// The pipeline never follows next-page links, and pagination
		// detection measurably slows small pages (#127).
		SkipPagination: true,
	})
	if err != nil {
		logger.Warn("extraction failed", "url", page.URL, "error", err)
		sentry.CaptureException(err)

		return Content{}, ErrExtractFailed
	}

	// The library never returns a nil Node: empty results come back as an
	// empty <div> wrapper, so the structural check alone would pass them.
	// Empty text is the real nothing-found signal.
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
