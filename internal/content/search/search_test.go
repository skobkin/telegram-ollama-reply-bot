package search

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"telegram-ollama-reply-bot/internal/config"
	"telegram-ollama-reply-bot/internal/provider"
)

func TestTavilySearcherSearch(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("unexpected method: %s", r.Method)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer tavily-token" {
			t.Fatalf("unexpected auth header: %q", got)
		}
		_, _ = io.WriteString(w, `{"query":"golang","results":[{"title":"Go","url":"https://go.dev","content":"The Go programming language","score":0.9,"published_date":"2026-04-20"}]}`)
	}))
	defer server.Close()

	original := tavilySearchURL
	tavilySearchURL = server.URL
	defer func() { tavilySearchURL = original }()

	result, err := NewTavilySearcher(server.Client(), "tavily-token", testLogger()).Search(context.Background(), Request{Query: "golang", MaxResults: 3})
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if result.Provider != "tavily" {
		t.Fatalf("unexpected provider: %q", result.Provider)
	}
	if len(result.Results) != 1 || result.Results[0].URL != "https://go.dev" {
		t.Fatalf("unexpected results: %+v", result.Results)
	}
}

func TestTavilySearcherClassifiesCreditsExhausted(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusPaymentRequired)
		_, _ = io.WriteString(w, `{"message":"Insufficient balance"}`)
	}))
	defer server.Close()

	original := tavilySearchURL
	tavilySearchURL = server.URL
	defer func() { tavilySearchURL = original }()

	_, err := NewTavilySearcher(server.Client(), "tavily-token", testLogger()).Search(context.Background(), Request{Query: "golang"})
	if kind := provider.KindOf(err); kind != provider.ErrorKindCreditsExhausted {
		t.Fatalf("unexpected error kind: %s", kind)
	}
}

func TestKagiSearcherSearch(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Fatalf("unexpected method: %s", r.Method)
		}
		if got := r.Header.Get("Authorization"); got != "Bot kagi-token" {
			t.Fatalf("unexpected auth header: %q", got)
		}
		if got := r.URL.Query().Get("limit"); got != "4" {
			t.Fatalf("unexpected limit: %q", got)
		}
		_, _ = io.WriteString(w, `{"meta":{"api_balance":123.45},"data":[{"t":0,"url":"https://kagi.com","title":"Kagi","snippet":"Premium search"},{"t":1,"list":["ignored"]}]}`)
	}))
	defer server.Close()

	original := kagiSearchURL
	kagiSearchURL = server.URL
	defer func() { kagiSearchURL = original }()

	result, err := NewKagiSearcher(server.Client(), "kagi-token", testLogger()).Search(context.Background(), Request{Query: "kagi", MaxResults: 4})
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if result.Provider != "kagi" {
		t.Fatalf("unexpected provider: %q", result.Provider)
	}
	if len(result.Results) != 1 || result.Results[0].Title != "Kagi" {
		t.Fatalf("unexpected results: %+v", result.Results)
	}
}

func TestKagiSearcherClassifiesErrorCodes(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"data":null,"error":[{"code":101,"msg":"Insufficient credit"}]}`)
	}))
	defer server.Close()

	original := kagiSearchURL
	kagiSearchURL = server.URL
	defer func() { kagiSearchURL = original }()

	_, err := NewKagiSearcher(server.Client(), "kagi-token", testLogger()).Search(context.Background(), Request{Query: "kagi"})
	if kind := provider.KindOf(err); kind != provider.ErrorKindCreditsExhausted {
		t.Fatalf("unexpected error kind: %s", kind)
	}
}

func TestChainSearcherFallsBackAndAggregatesErrors(t *testing.T) {
	t.Parallel()

	searcher := &ChainSearcher{
		searchers: []namedSearcher{
			{name: "kagi", searcher: stubSearcher{err: provider.NewError("kagi", provider.ErrorKindAuthFailed)}},
			{name: "tavily", searcher: stubSearcher{result: Result{Provider: "tavily", Query: "golang", Results: []ResultItem{{Title: "Go"}}}}},
		},
		logger: testLogger(),
	}

	result, err := searcher.Search(context.Background(), Request{Query: "golang"})
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if result.Provider != "tavily" {
		t.Fatalf("unexpected provider: %q", result.Provider)
	}

	allFail := &ChainSearcher{
		searchers: []namedSearcher{
			{name: "kagi", searcher: stubSearcher{err: provider.NewError("kagi", provider.ErrorKindAuthFailed)}},
			{name: "tavily", searcher: stubSearcher{err: provider.NewError("tavily", provider.ErrorKindCreditsExhausted)}},
		},
		logger: testLogger(),
	}
	_, err = allFail.Search(context.Background(), Request{Query: "golang"})
	if err == nil {
		t.Fatal("expected chain error")
	}
	if !strings.Contains(err.Error(), "kagi: auth_failed") || !strings.Contains(err.Error(), "tavily: credits_exhausted") {
		t.Fatalf("unexpected chain error: %v", err)
	}
}

func TestNewFromConfigDisablesSearchAndValidatesExplicitBackends(t *testing.T) {
	t.Parallel()

	cfg := config.Config{Search: config.SearchConfig{Backend: config.SearchBackendNone}}
	searcher, err := NewFromConfig(cfg, http.DefaultClient, testLogger())
	if err != nil {
		t.Fatalf("NewFromConfig() error = %v", err)
	}
	if searcher != nil {
		t.Fatalf("expected nil searcher, got %#v", searcher)
	}

	cfg = config.Config{Search: config.SearchConfig{Backend: config.SearchBackendTavily}}
	_, err = NewFromConfig(cfg, http.DefaultClient, testLogger())
	if kind := provider.KindOf(err); kind != provider.ErrorKindMisconfigured {
		t.Fatalf("unexpected error kind: %s", kind)
	}
}

type stubSearcher struct {
	result Result
	err    error
}

func (s stubSearcher) Search(context.Context, Request) (Result, error) {
	return s.result, s.err
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
