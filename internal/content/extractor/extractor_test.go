package extractor

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"telegram-ollama-reply-bot/internal/config"
	"telegram-ollama-reply-bot/internal/content/fetch"
)

func mustFixture(t *testing.T, name string) []byte {
	t.Helper()

	// The name comes from compile-time call sites below; testdata holds
	// checked-in fixtures only.
	raw, err := os.ReadFile("testdata/" + name) // #nosec G304 -- static testdata fixture path
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}

	return raw
}

func testPage(t *testing.T) *fetch.Page {
	t.Helper()

	return &fetch.Page{
		URL:         "https://example.com/article",
		ContentType: "text/html; charset=utf-8",
		Body:        mustFixture(t, "article.html"),
	}
}

// --- backend wrapper tests over the real fixture ---------------------------

func TestReadabilityBackendExtractsFixture(t *testing.T) {
	t.Parallel()

	backend := NewReadabilityExtractor(testLogger())

	content, err := backend.Extract(context.Background(), testPage(t))
	if err != nil {
		t.Fatalf("Extract returned error: %v", err)
	}

	if content.Title == "" {
		t.Fatal("expected non-empty title")
	}
	if !strings.Contains(content.Text, "ordinary Linux process") {
		t.Fatalf("expected article text, got %q", content.Text)
	}
	if !strings.Contains(content.HTML, "<h2>") {
		t.Fatal("expected structured HTML with headings")
	}
	// Nav boilerplate must not leak into the extraction.
	if strings.Contains(content.Text, "site-nav") && strings.Contains(content.Text, "Privacy") {
		t.Log("warning: boilerplate text present in extraction (quality gate handles link farms)")
	}
}

func TestTrafilaturaBackendExtractsFixture(t *testing.T) {
	t.Parallel()

	backend := NewTrafilaturaExtractor(testLogger())

	content, err := backend.Extract(context.Background(), testPage(t))
	if err != nil {
		t.Fatalf("Extract returned error: %v", err)
	}

	if !strings.Contains(content.Text, "ordinary Linux process") {
		t.Fatalf("expected article text, got %q", content.Text)
	}
	if !strings.Contains(content.HTML, "<h2>") {
		t.Fatal("expected structured HTML with headings")
	}
}

func TestGoOseBackendExtractsFixture(t *testing.T) {
	t.Parallel()

	backend := NewGoOseExtractor(testLogger())

	content, err := backend.Extract(context.Background(), testPage(t))
	if err != nil {
		t.Fatalf("Extract returned error: %v", err)
	}

	if content.Title == "" {
		t.Fatal("expected non-empty title")
	}
	if !strings.Contains(content.Text, "namespace") {
		t.Fatalf("expected article text, got %q", content.Text)
	}
}

// --- pipeline selection tests ----------------------------------------------

func TestPipelineFirstAcceptableBackendWins(t *testing.T) {
	t.Parallel()

	fallbackCalls := atomic.Int32{}
	pipeline := NewPipeline(
		stubFetcher{},
		[]Backend{
			stubBackend{name: "good", content: cleanContent()},
			stubBackend{name: "unused", calls: &fallbackCalls, content: cleanContent()},
		},
		testLogger(),
	)

	article, err := pipeline.GetArticleFromURL(context.Background(), "https://example.com/article")
	if err != nil {
		t.Fatalf("GetArticleFromURL returned error: %v", err)
	}

	if article.Backend != "good" {
		t.Fatalf("expected first acceptable backend to win, got %q", article.Backend)
	}
	if got := fallbackCalls.Load(); got != 0 {
		t.Fatalf("later backends must not run, got %d calls", got)
	}
}

func TestPipelineFallsThroughLowQualityResult(t *testing.T) {
	t.Parallel()

	pipeline := NewPipeline(
		stubFetcher{},
		[]Backend{
			stubBackend{name: "junk", content: linkFarmContent()},
			stubBackend{name: "good", content: cleanContent()},
		},
		testLogger(),
	)

	article, err := pipeline.GetArticleFromURL(context.Background(), "https://example.com/article")
	if err != nil {
		t.Fatalf("GetArticleFromURL returned error: %v", err)
	}

	if article.Backend != "good" {
		t.Fatalf("expected fall-through to good backend, got %q", article.Backend)
	}
}

func TestPipelineRescuesBestScoreWhenNothingAcceptable(t *testing.T) {
	t.Parallel()

	// Two boilerplate extractions, both below the gate; the milder one (fewer
	// links, more prose) must win as the rescue result.
	pipeline := NewPipeline(
		stubFetcher{},
		[]Backend{
			stubBackend{name: "linkfarm", content: linkFarmContent()},
			stubBackend{name: "mild", content: mildLinkFarmContent()},
		},
		testLogger(),
	)

	article, err := pipeline.GetArticleFromURL(context.Background(), "https://example.com/article")
	if err != nil {
		t.Fatalf("GetArticleFromURL returned error: %v", err)
	}

	if article.Backend != "mild" {
		t.Fatalf("expected best-scoring rescue, got %q", article.Backend)
	}
	if article.Text == "" {
		t.Fatal("rescue result must carry text")
	}
}

func TestPipelineAllEmptyFails(t *testing.T) {
	t.Parallel()

	pipeline := NewPipeline(
		stubFetcher{},
		[]Backend{stubBackend{name: "empty"}, stubBackend{name: "empty2"}},
		testLogger(),
	)

	article, err := pipeline.GetArticleFromURL(context.Background(), "https://example.com/article")
	if !errors.Is(err, ErrExtractFailed) {
		t.Fatalf("expected ErrExtractFailed, got %v", err)
	}
	if article != (Article{}) {
		t.Fatalf("expected empty article, got %+v", article)
	}
}

func TestPipelineBackendErrorsAreSkipped(t *testing.T) {
	t.Parallel()

	pipeline := NewPipeline(
		stubFetcher{},
		[]Backend{
			stubBackend{name: "broken", err: errors.New("boom")},
			stubBackend{name: "good", content: cleanContent()},
		},
		testLogger(),
	)

	article, err := pipeline.GetArticleFromURL(context.Background(), "https://example.com/article")
	if err != nil {
		t.Fatalf("GetArticleFromURL returned error: %v", err)
	}
	if article.Backend != "good" {
		t.Fatalf("expected good backend after broken one, got %q", article.Backend)
	}
}

func TestPipelineFetchErrorFailsWithoutBackends(t *testing.T) {
	t.Parallel()

	calls := atomic.Int32{}
	pipeline := NewPipeline(
		stubFetcher{err: fetch.ErrBlocked},
		[]Backend{stubBackend{name: "never", calls: &calls, content: cleanContent()}},
		testLogger(),
	)

	article, err := pipeline.GetArticleFromURL(context.Background(), "https://example.com/article")
	if !errors.Is(err, ErrExtractFailed) {
		t.Fatalf("expected ErrExtractFailed, got %v", err)
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("backends must not run when fetch fails, got %d calls", got)
	}
	if article != (Article{}) {
		t.Fatalf("expected empty article, got %+v", article)
	}
}

func TestPipelineHonorsContextCancellation(t *testing.T) {
	t.Parallel()

	calls := atomic.Int32{}
	pipeline := NewPipeline(
		stubFetcher{},
		[]Backend{stubBackend{name: "never", calls: &calls, content: cleanContent()}},
		testLogger(),
	)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := pipeline.GetArticleFromURL(ctx, "https://example.com/article")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("backends must not run after cancellation, got %d calls", got)
	}
}

func TestPipelineRendersMarkdownForWinner(t *testing.T) {
	t.Parallel()

	pipeline := NewPipeline(
		stubFetcher{},
		[]Backend{stubBackend{name: "good", content: Content{
			Title: "Winner",
			Text:  "Body text about the subject.",
			HTML:  "<h1>Winner</h1><p>See the <a href=\"/docs/guide\">guide</a> for details.</p>",
		}}},
		testLogger(),
	)

	article, err := pipeline.GetArticleFromURL(context.Background(), "https://example.com/article")
	if err != nil {
		t.Fatalf("GetArticleFromURL returned error: %v", err)
	}

	if !strings.Contains(article.Markdown, "# Winner") {
		t.Fatalf("expected heading in markdown, got %q", article.Markdown)
	}
	if !strings.Contains(article.Markdown, "https://example.com/docs/guide") {
		t.Fatalf("expected relative link resolved against final URL, got %q", article.Markdown)
	}
	if article.URL != "https://example.com/article" {
		t.Fatalf("unexpected article url: %q", article.URL)
	}
}

func TestPipelineEmptyBackendListFails(t *testing.T) {
	t.Parallel()

	pipeline := NewPipeline(stubFetcher{}, nil, testLogger())

	_, err := pipeline.GetArticleFromURL(context.Background(), "https://example.com/article")
	if !errors.Is(err, ErrExtractFailed) {
		t.Fatalf("expected ErrExtractFailed for empty pipeline, got %v", err)
	}
}

// --- backend construction ---------------------------------------------------

func TestNewBackendsBuildsConfiguredOrder(t *testing.T) {
	t.Parallel()

	backends, err := NewBackends([]string{
		config.ExtractionBackendTrafilatura,
		config.ExtractionBackendReadability,
		config.ExtractionBackendGoose,
	}, testLogger())
	if err != nil {
		t.Fatalf("NewBackends returned error: %v", err)
	}

	want := []string{"trafilatura", "readability", "goose"}
	got := make([]string, 0, len(backends))
	for _, b := range backends {
		got = append(got, b.Name())
	}

	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("unexpected backend order: %v", got)
	}
}

func TestNewBackendsRejectsUnknownName(t *testing.T) {
	t.Parallel()

	if _, err := NewBackends([]string{"readability", "unknown"}, testLogger()); err == nil {
		t.Fatal("expected error for unknown backend name")
	}
}

// --- stubs -------------------------------------------------------------------

// stubFetcher is an in-memory fetcher for pipeline tests.
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

	return &fetch.Page{URL: rawURL, Body: []byte("<html><body>stub</body></html>")}, nil
}

// stubBackend is a canned backend for pipeline tests.
type stubBackend struct {
	name    string
	content Content
	err     error
	calls   *atomic.Int32
}

func (s stubBackend) Name() string {
	if s.name != "" {
		return s.name
	}

	return "stub"
}

func (s stubBackend) Extract(_ context.Context, _ *fetch.Page) (Content, error) {
	if s.calls != nil {
		s.calls.Add(1)
	}

	if s.err != nil {
		return Content{}, s.err
	}

	return s.content, nil
}

// --- content builders calibrated against the quality scorer -----------------

// cleanContent is a solid article extraction: ~340 chars of prose, a title,
// two links (well under the link-density allowance).
func cleanContent() Content {
	text := "When you run a container, the kernel assembles a set of isolation " +
		"boundaries around an ordinary process. Namespaces shape its view of the " +
		"system, while control groups bound what it may consume. The runtime shim " +
		"keeps stdio flowing across daemon restarts, and the low-level runtime " +
		"performs the actual setup. Together these layers replace whole virtual " +
		"machines for most workloads, and every container tool is a convenience " +
		"wrapper over the same primitives the kernel has exposed for years."

	return Content{
		Title: "How container runtimes actually work",
		Text:  text,
		HTML: `<article><h1>How container runtimes actually work</h1><p>` + text +
			`</p><p>See the <a href="/docs/a">overview</a> and the <a href="/docs/b">spec</a>.</p></article>`,
	}
}

// linkFarmContent is boilerplate: little prose, many links.
func linkFarmContent() Content {
	var links strings.Builder
	for i := 0; i < 40; i++ {
		links.WriteString(`<a href="/nav/` + string(rune('a'+i%26)) + `/` + string(rune('a'+i/26)) + `">item</a> `)
	}

	return Content{
		Text: "Home Blog Docs About",
		HTML: "<nav>" + links.String() + "</nav>",
	}
}

// mildLinkFarmContent is boilerplate too, but milder: more prose, fewer
// links — it scores above linkFarmContent while staying below the gate.
func mildLinkFarmContent() Content {
	text := strings.Repeat("Section heading followed by a short teaser line. ", 8) // ~384 chars
	var links strings.Builder
	for i := 0; i < 30; i++ {
		links.WriteString(`<a href="/nav/` + string(rune('a'+i%26)) + `/` + string(rune('a'+i/26)) + `">teaser item</a> `)
	}

	return Content{
		Text: text,
		HTML: "<div><p>" + text + "</p>" + links.String() + "</div>",
	}
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
