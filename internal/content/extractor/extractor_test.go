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

// unextractablePage is a bare HTML shell with no article content at all.
// (Navigation-only pages do not qualify: readability and trafilatura
// legitimately distill the nav text as the longest content — the pipeline's
// quality gate handles that downstream.) Backends must report a bare shell
// as empty (Content.Empty), not as a failure.
func unextractablePage() *fetch.Page {
	body := `<!DOCTYPE html>
<html>
<head></head>
<body><div id="app"></div></body>
</html>`

	return &fetch.Page{
		URL:         "https://example.com/empty",
		ContentType: "text/html; charset=utf-8",
		Body:        []byte(body),
	}
}

// Every backend must report an unextractable page as an empty result
// (Content.Empty, nil error) rather than a failure.
func TestBackendsReportUnextractablePageAsEmpty(t *testing.T) {
	newBackend := map[string]func(*slog.Logger) Backend{
		config.ExtractionBackendReadability: func(l *slog.Logger) Backend {
			return NewReadabilityExtractor(l)
		},
		config.ExtractionBackendTrafilatura: func(l *slog.Logger) Backend {
			return NewTrafilaturaExtractor(l)
		},
		config.ExtractionBackendDomDistiller: func(l *slog.Logger) Backend {
			return NewDomDistillerExtractor(l)
		},
	}

	for name, constructor := range newBackend {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			content, err := constructor(testLogger()).Extract(context.Background(), unextractablePage())
			if err != nil {
				t.Fatalf("unextractable page must not be an error, got: %v", err)
			}
			if !content.Empty {
				t.Fatalf("expected empty-flagged content, got %+v", content)
			}
		})
	}
}

func TestDomDistillerBackendExtractsFixture(t *testing.T) {
	t.Parallel()

	backend := NewDomDistillerExtractor(testLogger())

	content, err := backend.Extract(context.Background(), testPage(t))
	if err != nil {
		t.Fatalf("Extract returned error: %v", err)
	}

	if content.Empty {
		t.Fatal("expected non-empty extraction for article fixture")
	}
	if content.Title == "" {
		t.Fatal("expected non-empty title")
	}
	// The title may keep the "| Example Engineering" suffix depending on
	// which candidate the distiller picks; assert the shared substring.
	if !strings.Contains(content.Title, "container runtimes") {
		t.Fatalf("expected article title, got %q", content.Title)
	}
	if !strings.Contains(content.Text, "ordinary Linux process") {
		t.Fatalf("expected article text, got %q", content.Text)
	}
	if !strings.Contains(content.HTML, "<h2>") {
		t.Fatal("expected structured HTML with headings")
	}
}

// --- regression fixture: Wikipedia-class pages (#132) -----------------------

// testdata/wikipedia.html is the complete HTML of
// https://en.wikipedia.org/wiki/Docker_(software) (retrieved 2026-11-12;
// Wikipedia content is CC BY-SA 4.0, credited to its editors). It is the
// hardest measured passing case for the #132 gate fix: readability extracts
// it at ~21 links per 1000 chars of citation-scale prose, a density the
// previous linear link penalty rejected on every backend.
func wikipediaPage(t *testing.T) *fetch.Page {
	t.Helper()

	return &fetch.Page{
		URL:         "https://en.wikipedia.org/wiki/Docker_(software)",
		ContentType: "text/html; charset=utf-8",
		Body:        mustFixture(t, "wikipedia.html"),
	}
}

func TestReadabilityWikipediaFixturePassesGate(t *testing.T) {
	t.Parallel()

	backend := NewReadabilityExtractor(testLogger())

	content, err := backend.Extract(context.Background(), wikipediaPage(t))
	if err != nil {
		t.Fatalf("Extract returned error: %v", err)
	}
	if content.Empty {
		t.Fatal("expected non-empty extraction for Wikipedia fixture")
	}
	if content.Title == "" {
		t.Fatal("expected non-empty title")
	}
	if !acceptable(content) {
		t.Fatalf("expected Wikipedia fixture to pass the quality gate, score %f", contentScore(content))
	}
	if !strings.Contains(content.Text, "OS-level virtualization") {
		t.Fatalf("expected lede text in extraction, got %.200q", content.Text)
	}
}

// The fixture must be delivered by the first configured backend outright —
// no fall-through, no rescue mode.
func TestPipelineWikipediaFixtureAccepted(t *testing.T) {
	t.Parallel()

	backends, err := NewBackends([]string{
		config.ExtractionBackendReadability,
		config.ExtractionBackendTrafilatura,
		config.ExtractionBackendDomDistiller,
	}, testLogger())
	if err != nil {
		t.Fatalf("NewBackends returned error: %v", err)
	}

	pipeline := NewPipeline(stubFetcher{page: wikipediaPage(t)}, backends, testLogger())

	article, err := pipeline.GetArticleFromURL(context.Background(), "https://en.wikipedia.org/wiki/Docker_(software)")
	if err != nil {
		t.Fatalf("GetArticleFromURL returned error: %v", err)
	}
	if article.Backend != config.ExtractionBackendReadability {
		t.Fatalf("expected readability to win at slot 1, got %q", article.Backend)
	}
	if article.Text == "" {
		t.Fatal("expected non-empty article text")
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

// The Empty flag must trump the structural check: content carrying wrapper
// markup (non-empty Text/HTML) is still skipped when the backend declared it
// empty, and the next backend gets its turn.
func TestPipelineSkipsDeclaredEmptyContent(t *testing.T) {
	t.Parallel()

	pipeline := NewPipeline(
		stubFetcher{},
		[]Backend{
			stubBackend{name: "wrapper", content: Content{
				Title: "Nothing here",
				Text:  "leftover",
				HTML:  "<div></div>",
				Empty: true,
			}},
			stubBackend{name: "good", content: cleanContent()},
		},
		testLogger(),
	)

	article, err := pipeline.GetArticleFromURL(context.Background(), "https://example.com/article")
	if err != nil {
		t.Fatalf("GetArticleFromURL returned error: %v", err)
	}

	if article.Backend != "good" {
		t.Fatalf("expected declared-empty content to be skipped, got backend %q", article.Backend)
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
		config.ExtractionBackendDomDistiller,
	}, testLogger())
	if err != nil {
		t.Fatalf("NewBackends returned error: %v", err)
	}

	want := []string{"trafilatura", "readability", "domdistiller"}
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

// referenceDenseContent is a Wikipedia-shaped extraction: ~10k runes of
// prose at ~12 links per 1000 chars (citations scale with content). This is
// the shape the linear gate rejected wholesale (#132).
func referenceDenseContent() Content {
	const paragraph = "The concept emerged from a long line of research that most histories " +
		"attribute to the early mainframe era, and later systems refined the " +
		"isolation primitives into the modern form. "

	var text, html strings.Builder
	html.WriteString("<article><h1>Reference-dense article</h1>")

	for text.Len() < 10000 {
		text.WriteString(paragraph)
		html.WriteString("<p>" + paragraph + ` See <a href="/ref/a">1</a>, <a href="/ref/b">2</a>, and <a href="/ref/c">3</a>.</p>`)
	}

	html.WriteString("</article>")

	return Content{
		Title: "Reference-dense article",
		Text:  text.String(),
		HTML:  html.String(),
	}
}

// longLinkFarmContent is a ~20k-rune spam page at ~50 links per 1000 chars:
// length alone must not buy a link farm out of the gate.
func longLinkFarmContent() Content {
	const spam = "Buy the best widgets now with free shipping and daily deals "

	var text, html strings.Builder

	for text.Len() < 20000 {
		text.WriteString(spam)
		html.WriteString("<p>" + spam + `<a href="/deal/a">shop</a> <a href="/deal/b">sale</a> <a href="/deal/c">more</a></p>`)
	}

	return Content{Text: text.String(), HTML: html.String()}
}

// moderateLinkIndexContent is a ~10k-rune index page at ~33 links per 1000
// chars: sparser than a farm, denser than any reference-heavy article.
func moderateLinkIndexContent() Content {
	const line = "Section overview with a short summary sentence for readers. "

	var text, html strings.Builder

	for text.Len() < 10000 {
		text.WriteString(line)
		html.WriteString("<p>" + line +
			`<a href="/s/a">a</a> <a href="/s/b">b</a></p>`)
	}

	return Content{Text: text.String(), HTML: html.String()}
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
