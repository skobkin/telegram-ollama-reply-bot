package extractor

import (
	"bytes"
	"context"
	"log/slog"

	"telegram-ollama-reply-bot/internal/content/fetch"
	"telegram-ollama-reply-bot/internal/logging"

	"github.com/PuerkitoBio/goquery"
	goose "github.com/advancedlogic/GoOse/pkg/goose"
	"github.com/getsentry/sentry-go"
	xhtml "golang.org/x/net/html"
)

// GoOseExtractor is the GoOse-based extraction backend. It runs synchronously
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

func (e *GoOseExtractor) Name() string {
	return "goose"
}

func (e *GoOseExtractor) Extract(ctx context.Context, page *fetch.Page) (Content, error) {
	logger := logging.FromContext(ctx, e.logger)
	logger.Debug("extracting article from fetched page", "url", page.URL)

	article, err := e.goose.ExtractFromRawHTML(string(page.Body), page.URL)
	if err != nil {
		logger.Warn("extraction failed", "url", page.URL, "error", err)
		sentry.CaptureException(err)

		return Content{}, ErrExtractFailed
	}

	logger.Debug("article extracted", "url", page.URL, "text_length", len(article.CleanedText))

	return Content{
		Title: article.Title,
		Text:  article.CleanedText,
		HTML:  renderNodesOuterHTML(article.TopNode),
	}, nil
}

// renderNodesOuterHTML renders a goquery selection's nodes back to an HTML
// string (outer form) for Markdown rendering. The selection may be nil or
// empty; the result is then empty too.
func renderNodesOuterHTML(selection *goquery.Selection) string {
	if selection == nil {
		return ""
	}

	var buf bytes.Buffer
	for _, node := range selection.Nodes {
		if node == nil {
			continue
		}
		if err := xhtml.Render(&buf, node); err != nil {
			continue
		}
	}

	return buf.String()
}
