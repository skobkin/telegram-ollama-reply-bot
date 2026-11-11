package extractor

import (
	"reflect"
	"testing"

	"telegram-ollama-reply-bot/internal/config"
	"telegram-ollama-reply-bot/internal/content/fetch"
)

// realBackends returns the three production backends in the given order so
// evaluator vote keys and backend names can never drift apart.
func realBackends(t *testing.T, names []string) []Backend {
	t.Helper()

	backends, err := NewBackends(names, testLogger())
	if err != nil {
		t.Fatalf("NewBackends returned error: %v", err)
	}

	return backends
}

func backendNames(backends []Backend) []string {
	names := make([]string, 0, len(backends))
	for _, b := range backends {
		names = append(names, b.Name())
	}

	return names
}

func TestExtractFeatures(t *testing.T) {
	t.Parallel()

	page := &fetch.Page{
		URL:  "https://WWW.Example.com/page",
		Body: []byte(`<html><body><div id="mw-content-text"><p>hi</p></div></body></html>`),
	}

	f := extractFeatures(page)

	if f.Host != "example.com" {
		t.Fatalf("expected stripped lowercase host, got %q", f.Host)
	}
	if !f.MediaWiki {
		t.Fatal("expected MediaWiki marker detection from body")
	}
	if f.BodySize != len(page.Body) {
		t.Fatalf("expected body size %d, got %d", len(page.Body), f.BodySize)
	}
	if f.URL == nil || f.URL.Host != "WWW.Example.com" {
		t.Fatalf("expected parsed URL, got %v", f.URL)
	}
}

func TestExtractFeaturesWikiHost(t *testing.T) {
	t.Parallel()

	page := &fetch.Page{URL: "https://somewiki.example.org/page", Body: []byte("<html></html>")}

	if f := extractFeatures(page); !f.MediaWiki {
		t.Fatalf("expected wiki host to set MediaWiki, got %+v", f)
	}
}

func TestExtractFeaturesUnparseableURL(t *testing.T) {
	t.Parallel()

	page := &fetch.Page{URL: ":://not-a-url", Body: []byte("plain")}

	f := extractFeatures(page)

	if f.URL != nil {
		t.Fatalf("expected nil URL for unparseable input, got %v", f.URL)
	}
	if f.Host != "" {
		t.Fatalf("expected empty host, got %q", f.Host)
	}
	if f.MediaWiki {
		t.Fatal("expected no MediaWiki detection")
	}
}

func TestExtractFeaturesArticleLargeIsNeutral(t *testing.T) {
	t.Parallel()

	page := &fetch.Page{
		URL:  "https://example.com/article_large.html",
		Body: mustFixture(t, "article_large.html"),
	}

	f := extractFeatures(page)

	if f.BodySize != 142267 {
		t.Fatalf("expected body size 142267, got %d", f.BodySize)
	}
	if f.MediaWiki {
		t.Fatal("expected neutral fixture to carry no MediaWiki markers")
	}
}

func TestSelectorNeutralFeaturesKeepConfiguredOrder(t *testing.T) {
	t.Parallel()

	backends := realBackends(t, []string{
		config.ExtractionBackendTrafilatura,
		config.ExtractionBackendReadability,
		config.ExtractionBackendDomDistiller,
	})

	sel := newSelector()
	o := sel.order(Features{Host: "example.com", BodySize: 3000}, backends)

	if !reflect.DeepEqual(backendNames(o.Backends), backendNames(backends)) {
		t.Fatalf("expected configured order, got %v", backendNames(o.Backends))
	}
	if len(o.Votes) != 0 {
		t.Fatalf("expected no votes, got %v", o.Votes)
	}
	if len(o.Fired) != 0 {
		t.Fatalf("expected no evaluators fired, got %v", o.Fired)
	}
}

func TestSelectorMediaWikiVotes(t *testing.T) {
	t.Parallel()

	backends := realBackends(t, []string{
		config.ExtractionBackendDomDistiller,
		config.ExtractionBackendTrafilatura,
		config.ExtractionBackendReadability,
	})

	o := newSelector().order(Features{MediaWiki: true, Host: "en.wikipedia.org"}, backends)

	want := []string{
		config.ExtractionBackendReadability,
		config.ExtractionBackendTrafilatura,
		config.ExtractionBackendDomDistiller,
	}
	if !reflect.DeepEqual(backendNames(o.Backends), want) {
		t.Fatalf("expected readability promoted on wiki page, got %v (votes %v)", backendNames(o.Backends), o.Votes)
	}
	if !reflect.DeepEqual(o.Fired, []string{"mediawiki"}) {
		t.Fatalf("expected mediawiki evaluator fired, got %v", o.Fired)
	}
}

func TestSelectorIgnoresVotesForUnconfiguredBackends(t *testing.T) {
	t.Parallel()

	// readability is voted on wiki pages but not configured: its votes must
	// be ignored and only the demotion must apply.
	backends := realBackends(t, []string{
		config.ExtractionBackendDomDistiller,
		config.ExtractionBackendTrafilatura,
	})

	o := newSelector().order(Features{MediaWiki: true, Host: "en.wikipedia.org"}, backends)

	want := []string{
		config.ExtractionBackendTrafilatura,
		config.ExtractionBackendDomDistiller,
	}
	if !reflect.DeepEqual(backendNames(o.Backends), want) {
		t.Fatalf("expected unregistered votes ignored, got %v (votes %v)", backendNames(o.Backends), o.Votes)
	}
}

func TestSelectorLongPageVotes(t *testing.T) {
	t.Parallel()

	backends := realBackends(t, []string{
		config.ExtractionBackendReadability,
		config.ExtractionBackendTrafilatura,
		config.ExtractionBackendDomDistiller,
	})

	o := newSelector().order(Features{Host: "example.com", BodySize: 200 << 10}, backends)

	want := []string{
		config.ExtractionBackendDomDistiller,
		config.ExtractionBackendReadability,
		config.ExtractionBackendTrafilatura,
	}
	if !reflect.DeepEqual(backendNames(o.Backends), want) {
		t.Fatalf("expected domdistiller promoted on long page, got %v (votes %v)", backendNames(o.Backends), o.Votes)
	}
	if !reflect.DeepEqual(o.Fired, []string{"long-page"}) {
		t.Fatalf("expected long-page evaluator fired, got %v", o.Fired)
	}
}

func TestSelectorCombinedWikiAndLongPage(t *testing.T) {
	t.Parallel()

	// A very large wiki article: readability +1 (wiki), domdistiller -1+1=0,
	// trafilatura -1 (long page) -> readability leads, domdistiller next.
	backends := realBackends(t, []string{
		config.ExtractionBackendReadability,
		config.ExtractionBackendTrafilatura,
		config.ExtractionBackendDomDistiller,
	})

	features := Features{MediaWiki: true, Host: "en.wikipedia.org", BodySize: 300 << 10}

	o := newSelector().order(features, backends)

	want := []string{
		config.ExtractionBackendReadability,
		config.ExtractionBackendDomDistiller,
		config.ExtractionBackendTrafilatura,
	}
	if !reflect.DeepEqual(backendNames(o.Backends), want) {
		t.Fatalf("expected combined votes order, got %v (votes %v)", backendNames(o.Backends), o.Votes)
	}
	if !reflect.DeepEqual(o.Fired, []string{"mediawiki", "long-page"}) {
		t.Fatalf("expected both evaluators fired, got %v", o.Fired)
	}
}

// tieEvaluator votes equal positive weights on two backends: the tie must
// resolve to the configured relative order.
type tieEvaluator struct{}

func (tieEvaluator) Name() string { return "tie" }

func (tieEvaluator) Vote(_ Features) map[string]float64 {
	return map[string]float64{
		config.ExtractionBackendTrafilatura: 1.0,
		config.ExtractionBackendReadability: 1.0,
	}
}

func TestSelectorTiesKeepConfiguredOrder(t *testing.T) {
	t.Parallel()

	backends := realBackends(t, []string{
		config.ExtractionBackendTrafilatura,
		config.ExtractionBackendReadability,
		config.ExtractionBackendDomDistiller,
	})

	sel := &selector{evaluators: []Evaluator{tieEvaluator{}}}
	o := sel.order(Features{Host: "example.com"}, backends)

	if !reflect.DeepEqual(backendNames(o.Backends), backendNames(backends)) {
		t.Fatalf("expected tied votes to keep configured order, got %v", backendNames(o.Backends))
	}
}

func TestOrderingNames(t *testing.T) {
	t.Parallel()

	backends := realBackends(t, []string{config.ExtractionBackendReadability})

	o := Ordering{Backends: backends}

	if !reflect.DeepEqual(o.names(), []string{"readability"}) {
		t.Fatalf("unexpected names: %v", o.names())
	}
}
