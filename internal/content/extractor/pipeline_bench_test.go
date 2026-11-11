package extractor

import (
	"context"
	"log/slog"
	"os"
	"testing"

	"telegram-ollama-reply-bot/internal/config"
	"telegram-ollama-reply-bot/internal/content/fetch"
)

// Benchmarks measure per-backend CPU cost and allocations over checked-in
// real-world-shaped fixtures — regression visibility for extraction, not
// latency claims (the fetch path is network-bound and covered by the fetcher
// tests). Run with:
//
//	go test -bench=. -benchmem -run=^$ ./internal/content/extractor/
func benchPage(tb testing.TB, name string) *fetch.Page {
	tb.Helper()

	// The name comes from compile-time call sites below; testdata holds
	// checked-in fixtures only.
	raw, err := os.ReadFile("testdata/" + name) // #nosec G304 -- static testdata fixture path
	if err != nil {
		tb.Fatalf("read fixture %s: %v", name, err)
	}

	return &fetch.Page{
		URL:         "https://example.com/" + name,
		ContentType: "text/html; charset=utf-8",
		Body:        raw,
	}
}

func benchLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.NewFile(0, os.DevNull), nil))
}

func BenchmarkReadabilityBackend(b *testing.B) {
	page := benchPage(b, "article.html")
	backend := NewReadabilityExtractor(benchLogger())
	ctx := context.Background()

	b.ReportAllocs()
	b.ResetTimer()

	for range b.N {
		if _, err := backend.Extract(ctx, page); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkTrafilaturaBackend(b *testing.B) {
	page := benchPage(b, "article.html")
	backend := NewTrafilaturaExtractor(benchLogger())
	ctx := context.Background()

	b.ReportAllocs()
	b.ResetTimer()

	for range b.N {
		if _, err := backend.Extract(ctx, page); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkDomDistillerBackend(b *testing.B) {
	page := benchPage(b, "article.html")
	backend := NewDomDistillerExtractor(benchLogger())
	ctx := context.Background()

	b.ReportAllocs()
	b.ResetTimer()

	for range b.N {
		if _, err := backend.Extract(ctx, page); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkReadabilityBackendLarge(b *testing.B) {
	page := benchPage(b, "article_large.html")
	backend := NewReadabilityExtractor(benchLogger())
	ctx := context.Background()

	b.ReportAllocs()
	b.ResetTimer()

	for range b.N {
		if _, err := backend.Extract(ctx, page); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkTrafilaturaBackendLarge(b *testing.B) {
	page := benchPage(b, "article_large.html")
	backend := NewTrafilaturaExtractor(benchLogger())
	ctx := context.Background()

	b.ReportAllocs()
	b.ResetTimer()

	for range b.N {
		if _, err := backend.Extract(ctx, page); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkDomDistillerBackendLarge(b *testing.B) {
	page := benchPage(b, "article_large.html")
	backend := NewDomDistillerExtractor(benchLogger())
	ctx := context.Background()

	b.ReportAllocs()
	b.ResetTimer()

	for range b.N {
		if _, err := backend.Extract(ctx, page); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkPipeline(b *testing.B) {
	// Full pipeline: the fetch is stubbed, all default backends run in
	// configured order, and the winner is rendered to Markdown — the
	// per-request CPU floor for /summarize and fetch_url_content.
	page := benchPage(b, "article.html")
	backends, err := NewBackends([]string{
		config.ExtractionBackendReadability,
		config.ExtractionBackendTrafilatura,
		config.ExtractionBackendDomDistiller,
	}, benchLogger())
	if err != nil {
		b.Fatal(err)
	}

	pipeline := NewPipeline(stubFetcher{page: page}, backends, benchLogger())
	ctx := context.Background()

	b.ReportAllocs()
	b.ResetTimer()

	for range b.N {
		if _, err := pipeline.GetArticleFromURL(ctx, "https://example.com/article"); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkPipelineWikipedia(b *testing.B) {
	// Full pipeline over the Wikipedia fixture (#132): both page evaluators
	// fire (MediaWiki markers in the body, 383 KB body), the order is
	// re-ranked, and readability delivers a gate-passing extraction.
	page := benchPage(b, "wikipedia.html")
	backends, err := NewBackends([]string{
		config.ExtractionBackendReadability,
		config.ExtractionBackendTrafilatura,
		config.ExtractionBackendDomDistiller,
	}, benchLogger())
	if err != nil {
		b.Fatal(err)
	}

	pipeline := NewPipeline(stubFetcher{page: page}, backends, benchLogger())
	ctx := context.Background()

	b.ReportAllocs()
	b.ResetTimer()

	for range b.N {
		if _, err := pipeline.GetArticleFromURL(ctx, "https://en.wikipedia.org/wiki/Docker_(software)"); err != nil {
			b.Fatal(err)
		}
	}
}
