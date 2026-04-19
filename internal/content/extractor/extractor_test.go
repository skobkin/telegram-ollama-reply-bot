package extractor

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestReadabilityExtractorGetArticleFromURL(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `<!doctype html>
<html>
<head><title>Readable title</title></head>
<body>
<article>
<h1>Readable title</h1>
<p>First paragraph.</p>
<p>Second paragraph.</p>
</article>
</body>
</html>`)
	}))
	defer server.Close()

	extractor := NewReadabilityExtractor(testLogger())

	article, err := extractor.GetArticleFromURL(context.Background(), server.URL)
	if err != nil {
		t.Fatalf("GetArticleFromURL returned error: %v", err)
	}

	if article.Title != "Readable title" {
		t.Fatalf("unexpected title: %q", article.Title)
	}

	if article.URL != server.URL {
		t.Fatalf("unexpected url: %q", article.URL)
	}

	if !strings.Contains(article.Text, "First paragraph.") {
		t.Fatalf("expected extracted text to contain first paragraph, got %q", article.Text)
	}

	if !strings.Contains(article.Text, "Second paragraph.") {
		t.Fatalf("expected extracted text to contain second paragraph, got %q", article.Text)
	}
}

func TestMultiExtractorReturnsPrimaryArticle(t *testing.T) {
	t.Parallel()

	fallbackCalls := atomic.Int32{}
	extractor := &MultiExtractor{
		primary: stubExtractor{
			article: Article{Title: "primary", Text: "primary text", URL: "https://example.com"},
		},
		fallback: stubExtractor{
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
		primary: stubExtractor{
			err: errors.New("boom"),
		},
		fallback: stubExtractor{
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
		primary: stubExtractor{
			article: Article{Title: "primary", Text: "", URL: "https://example.com"},
		},
		fallback: stubExtractor{
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

type stubExtractor struct {
	article Article
	err     error
	calls   *atomic.Int32
}

func (s stubExtractor) GetArticleFromURL(context.Context, string) (Article, error) {
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
