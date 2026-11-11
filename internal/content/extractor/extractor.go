package extractor

import (
	"context"
	"errors"
	"log/slog"
	"telegram-ollama-reply-bot/internal/content/fetch"
	"telegram-ollama-reply-bot/internal/logging"

	"github.com/getsentry/sentry-go"
)

var (
	ErrExtractFailed = errors.New("extraction failed")
)

// Article is the extraction result delivered to consumers. Text is the plain
// rendering of the extracted article; URL is the final URL the content was
// fetched from (post-redirect).
type Article struct {
	Title string
	Text  string
	URL   string
}

// Extractor is the consumer-facing extraction API: it takes a URL and does
// everything needed to return the article — fetching (via the shared SSRF
// policy fetcher) and content extraction (via the configured backends).
type Extractor interface {
	GetArticleFromURL(ctx context.Context, url string) (Article, error)
}

// pageFetcher is the fetch boundary the extractor depends on. It matches
// *fetch.Fetcher; tests replace it with a stub.
type pageFetcher interface {
	Fetch(ctx context.Context, rawURL string) (*fetch.Page, error)
}

// pageBackend is one extraction backend: a CPU-only pass over an
// already-fetched page. Backends never perform network IO.
type pageBackend interface {
	name() string
	extractPage(ctx context.Context, page *fetch.Page) (Article, error)
}

// MultiExtractor fetches once and runs a primary backend with a fallback
// backend over the fetched page.
type MultiExtractor struct {
	fetcher  pageFetcher
	primary  pageBackend
	fallback pageBackend
	logger   *slog.Logger
}

// NewExtractor builds the extraction stack: a readability primary with a
// GoOse fallback, both fed by the shared fetcher.
func NewExtractor(fetcher pageFetcher, logger *slog.Logger) Extractor {
	if logger == nil {
		logger = slog.Default().With("pkg", "content/extractor")
	}

	return &MultiExtractor{
		fetcher:  fetcher,
		primary:  NewReadabilityExtractor(logger.With("backend", "readability")),
		fallback: NewGoOseExtractor(logger.With("backend", "goose")),
		logger:   logger,
	}
}

// GetArticleFromURL fetches the URL once through the shared policy fetcher
// and extracts the article with the primary backend, falling back on error
// or empty text.
func (e *MultiExtractor) GetArticleFromURL(ctx context.Context, url string) (Article, error) {
	logger := e.logger
	if ctx != nil {
		logger = logging.FromContext(ctx, e.logger).With("backend", "multi")
	}

	logger.Info("extracting article", "url", url)

	page, err := e.fetcher.Fetch(ctx, url)
	if err != nil {
		logger.Warn("page fetch failed", "url", url, "error", err)
		sentry.CaptureException(err)

		return Article{}, ErrExtractFailed
	}

	article, err := e.primary.extractPage(ctx, page)
	if err == nil && article.Text != "" {
		logger.Info("article extracted with primary backend", "text_length", len(article.Text))

		return article, nil
	} else if err != nil {
		logger.Warn("primary extractor failed", "url", url, "error", err)
		sentry.CaptureException(err)
	}

	logger.Info("trying fallback extractor", "url", url)
	article, err = e.fallback.extractPage(ctx, page)
	if err == nil && article.Text != "" {
		logger.Info("article extracted with fallback backend", "text_length", len(article.Text))

		return article, nil
	} else if err != nil {
		logger.Warn("fallback extractor failed", "url", url, "error", err)
		sentry.CaptureException(err)
	}

	logger.Error("all extractors failed", "url", url)

	return Article{}, ErrExtractFailed
}
