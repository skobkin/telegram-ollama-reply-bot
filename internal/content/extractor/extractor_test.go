package extractor

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"

	"telegram-ollama-reply-bot/internal/content/fetch"
)

const readablePage = `<!doctype html>
<html>
<head><title>Readable title</title></head>
<body>
<article>
<h1>Readable title</h1>
<p>First paragraph.</p>
<p>Second paragraph.</p>
</article>
</body>
</html>`

func TestReadabilityExtractorExtractsPage(t *testing.T) {
	t.Parallel()

	extractor := NewReadabilityExtractor(testLogger())

	article, err := extractor.extractPage(context.Background(), &fetch.Page{
		URL:  "https://example.com/article",
		Body: []byte(readablePage),
	})
	if err != nil {
		t.Fatalf("extractPage returned error: %v", err)
	}

	if article.Title != "Readable title" {
		t.Fatalf("unexpected title: %q", article.Title)
	}

	if article.URL != "https://example.com/article" {
		t.Fatalf("unexpected url: %q", article.URL)
	}

	if !strings.Contains(article.Text, "First paragraph.") {
		t.Fatalf("expected extracted text to contain first paragraph, got %q", article.Text)
	}

	if !strings.Contains(article.Text, "Second paragraph.") {
		t.Fatalf("expected extracted text to contain second paragraph, got %q", article.Text)
	}
}

func TestGoOseExtractorExtractsPage(t *testing.T) {
	t.Parallel()

	extractor := NewGoOseExtractor(testLogger())

	article, err := extractor.extractPage(context.Background(), &fetch.Page{
		URL:  "https://example.com/article",
		Body: []byte(readablePage),
	})
	if err != nil {
		t.Fatalf("extractPage returned error: %v", err)
	}

	if article.Title != "Readable title" {
		t.Fatalf("unexpected title: %q", article.Title)
	}

	if article.URL != "https://example.com/article" {
		t.Fatalf("unexpected url: %q", article.URL)
	}

	if !strings.Contains(article.Text, "First paragraph.") {
		t.Fatalf("expected extracted text to contain first paragraph, got %q", article.Text)
	}
}

func TestMultiExtractorReturnsPrimaryArticle(t *testing.T) {
	t.Parallel()

	fallbackCalls := atomic.Int32{}
	extractor := &MultiExtractor{
		fetcher: stubFetcher{},
		primary: stubBackend{
			article: Article{Title: "primary", Text: "primary text", URL: "https://example.com"},
		},
		fallback: stubBackend{
			calls:   &fallbackCalls,
			article: Article{Title: "fallback", Text: "fallback text", URL: "https://example.com"},
		},
		logger: testLogger(),
	}

	article, err := extractor.GetArticleFromURL(context.Background(), "https://example.com")
	if err != nil {
		t.Fatalf("GetArticleFromURL returned error: %v", err)
	}

	if article.Title != "primary" {
		t.Fatalf("unexpected title: %q", article.Title)
	}

	if got := fallbackCalls.Load(); got != 0 {
		t.Fatalf("fallback should not be called, got %d calls", got)
	}
}

func TestMultiExtractorFallsBackWhenPrimaryFails(t *testing.T) {
	t.Parallel()

	fallbackCalls := atomic.Int32{}
	extractor := &MultiExtractor{
		fetcher: stubFetcher{},
		primary: stubBackend{
			err: errors.New("boom"),
		},
		fallback: stubBackend{
			calls:   &fallbackCalls,
			article: Article{Title: "fallback", Text: "fallback text", URL: "https://example.com"},
		},
		logger: testLogger(),
	}

	article, err := extractor.GetArticleFromURL(context.Background(), "https://example.com")
	if err != nil {
		t.Fatalf("GetArticleFromURL returned error: %v", err)
	}

	if article.Title != "fallback" {
		t.Fatalf("unexpected title: %q", article.Title)
	}

	if got := fallbackCalls.Load(); got != 1 {
		t.Fatalf("fallback should be called once, got %d calls", got)
	}
}

func TestMultiExtractorFallsBackWhenPrimaryTextIsEmpty(t *testing.T) {
	t.Parallel()

	fallbackCalls := atomic.Int32{}
	extractor := &MultiExtractor{
		fetcher: stubFetcher{},
		primary: stubBackend{
			article: Article{Title: "primary", Text: "", URL: "https://example.com"},
		},
		fallback: stubBackend{
			calls:   &fallbackCalls,
			article: Article{Title: "fallback", Text: "fallback text", URL: "https://example.com"},
		},
		logger: testLogger(),
	}

	article, err := extractor.GetArticleFromURL(context.Background(), "https://example.com")
	if err != nil {
		t.Fatalf("GetArticleFromURL returned error: %v", err)
	}

	if article.Title != "fallback" {
		t.Fatalf("unexpected title: %q", article.Title)
	}

	if got := fallbackCalls.Load(); got != 1 {
		t.Fatalf("fallback should be called once, got %d calls", got)
	}
}

func TestMultiExtractorFetchErrorPropagates(t *testing.T) {
	t.Parallel()

	primaryCalls := atomic.Int32{}
	extractor := &MultiExtractor{
		fetcher: stubFetcher{
			err: fetch.ErrBlocked,
		},
		primary: stubBackend{
			calls: &primaryCalls,
		},
		fallback: stubBackend{},
		logger:   testLogger(),
	}

	article, err := extractor.GetArticleFromURL(context.Background(), "https://example.com")
	if !errors.Is(err, ErrExtractFailed) {
		t.Fatalf("expected ErrExtractFailed, got %v", err)
	}

	if article != (Article{}) {
		t.Fatalf("expected empty article, got %+v", article)
	}

	if got := primaryCalls.Load(); got != 0 {
		t.Fatalf("backends must not run when the fetch fails, got %d primary calls", got)
	}
}

// stubFetcher is an in-memory pageFetcher for pipeline tests.
type stubFetcher struct {
	page *fetch.Page
	err  error
}

func (s stubFetcher) Fetch(_ context.Context, rawURL string) (*fetch.Page, error) {
	if s.err != nil {
		return nil, s.err
	}
	if s.page != nil {
		return s.page, nil
	}

	return &fetch.Page{URL: rawURL, Body: []byte(readablePage)}, nil
}

// stubBackend is a canned pageBackend for pipeline tests.
type stubBackend struct {
	article Article
	err     error
	calls   *atomic.Int32
}

func (s stubBackend) name() string {
	return "stub"
}

func (s stubBackend) extractPage(_ context.Context, _ *fetch.Page) (Article, error) {
	if s.calls != nil {
		s.calls.Add(1)
	}

	if s.err != nil {
		return Article{}, s.err
	}

	return s.article, nil
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
