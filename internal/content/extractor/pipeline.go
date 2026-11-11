package extractor

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"telegram-ollama-reply-bot/internal/content/fetch"
	"telegram-ollama-reply-bot/internal/logging"

	"github.com/getsentry/sentry-go"
)

// ErrExtractFailed is returned when the pipeline could not produce any
// usable article.
var ErrExtractFailed = errors.New("extraction failed")

// Fetcher is the fetch dependency of the pipeline; *fetch.Fetcher satisfies
// it, tests can inject stubs.
type Fetcher interface {
	Fetch(ctx context.Context, rawURL string) (*fetch.Page, error)
}

// Article is the extraction result delivered to consumers.
//
// Text is the plain rendering (kept for compatibility); Markdown carries the
// structure-preserving rendering derived from the winning extraction and is
// the preferred content for LLM consumption. URL is the final URL the page
// was fetched from (post-redirect); Backend names the engine that won.
type Article struct {
	Title    string
	Text     string
	Markdown string
	URL      string
	Backend  string
}

// Extractor is the consumer-facing extraction API: it takes a URL and does
// everything needed to return the article — fetching (via the shared SSRF
// policy fetcher) and content extraction (via the configured backends).
type Extractor interface {
	GetArticleFromURL(ctx context.Context, url string) (Article, error)
}

// Pipeline fetches once and runs the configured extraction backends in order
// over the fetched page. The first result passing the quality gate wins;
// when none does, the highest-scoring non-empty result is delivered as a
// rescue instead of failing.
type Pipeline struct {
	fetcher  Fetcher
	backends []Backend
	logger   *slog.Logger
}

// NewPipeline composes the extraction stack.
func NewPipeline(fetcher Fetcher, backends []Backend, logger *slog.Logger) *Pipeline {
	if logger == nil {
		logger = slog.Default().With("pkg", "content/extractor")
	}

	return &Pipeline{
		fetcher:  fetcher,
		backends: backends,
		logger:   logger,
	}
}

// GetArticleFromURL fetches the URL once through the shared policy fetcher
// and extracts the article with the configured backend pipeline.
func (p *Pipeline) GetArticleFromURL(ctx context.Context, url string) (Article, error) {
	logger := p.logger
	if ctx != nil {
		logger = logging.FromContext(ctx, p.logger).With("backend", "pipeline")
	}

	logger.Info("extracting article", "url", url)

	if len(p.backends) == 0 {
		return Article{}, ErrExtractFailed
	}

	page, err := p.fetcher.Fetch(ctx, url)
	if err != nil {
		logger.Warn("page fetch failed", "url", url, "error", err)
		sentry.CaptureException(err)

		return Article{}, ErrExtractFailed
	}

	var (
		best      Content
		bestScore = math.Inf(-1) // any non-empty result can be rescued
		bestName  string
	)

	for _, backend := range p.backends {
		// Cancellation surfaces between CPU stages; backends themselves do
		// not spawn goroutines, so there is nothing to leak.
		if err := ctx.Err(); err != nil {
			logger.Warn("extraction canceled", "url", url, "error", err)

			return Article{}, err
		}

		name := backend.Name()
		content, err := backend.Extract(ctx, page)
		if err != nil {
			// The backend already logged and reported the underlying cause;
			// the sentinel error carries no context, so capturing it here
			// would only duplicate the event.
			logger.Warn("backend extraction failed", "backend", name, "url", url, "error", err)

			continue
		}
		// Emptiness is the producing backend's call first (some engines render
		// wrapper markup even for empty pages); structure is the fallback
		// check for results that leave the flag unset.
		if content.Empty || !content.nonEmpty() {
			logger.Debug("backend produced empty content", "backend", name, "url", url)

			continue
		}

		score := contentScore(content)
		logger.Debug("backend scored", "backend", name, "score", score, "text_length", len(content.Text))

		if acceptable(content) {
			logger.Info("article extracted", "backend", name, "text_length", len(content.Text))

			return p.buildArticle(content, page.URL, name, logger)
		}

		if score > bestScore {
			best, bestScore, bestName = content, score, name
		}
	}

	// Rescue: nothing passed the gate, but a non-empty result exists — a
	// weak extraction still beats failing the request outright.
	if best.nonEmpty() {
		logger.Info("no backend passed the quality gate, using best result", "backend", bestName, "score", bestScore)

		return p.buildArticle(best, page.URL, bestName, logger)
	}

	logger.Error("all extractors failed", "url", url)

	return Article{}, ErrExtractFailed
}

// buildArticle renders the winning content into the consumer-facing Article.
// A Markdown rendering failure never fails the request: Text is still
// delivered and the error is logged.
func (p *Pipeline) buildArticle(
	content Content,
	finalURL string,
	backendName string,
	logger *slog.Logger,
) (Article, error) {
	markdown, err := renderMarkdown(content.HTML, finalURL)
	if err != nil {
		logger.Warn("markdown rendering failed", "backend", backendName, "url", finalURL, "error", err)
		sentry.CaptureException(err)
	}

	return Article{
		Title:    content.Title,
		Text:     content.Text,
		Markdown: markdown,
		URL:      finalURL,
		Backend:  backendName,
	}, nil
}
