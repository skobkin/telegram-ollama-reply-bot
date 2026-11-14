package search

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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

	searcher := NewTavilySearcher(server.Client(), "tavily-token", testLogger())
	searcher.searchURL = server.URL

	result, err := searcher.Search(context.Background(), Request{Query: "golang", MaxResults: 3})
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

	searcher := NewTavilySearcher(server.Client(), "tavily-token", testLogger())
	searcher.searchURL = server.URL

	_, err := searcher.Search(context.Background(), Request{Query: "golang"})
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

	searcher := NewKagiSearcher(server.Client(), "kagi-token", testLogger())
	searcher.searchURL = server.URL

	result, err := searcher.Search(context.Background(), Request{Query: "kagi", MaxResults: 4})
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

	searcher := NewKagiSearcher(server.Client(), "kagi-token", testLogger())
	searcher.searchURL = server.URL

	_, err := searcher.Search(context.Background(), Request{Query: "kagi"})
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

// stubAuthSearcher is a Searcher with an independent ValidateAuth outcome,
// used to test chain aggregation without HTTP. When validate is set it takes
// precedence — use it for context-sensitive stubs (a real provider fails with
// the context's error once its deadline has expired).
type stubAuthSearcher struct {
	stubSearcher
	validateErr error
	validate    func(context.Context) error
}

func (s stubAuthSearcher) ValidateAuth(ctx context.Context) error {
	if s.validate != nil {
		return s.validate(ctx)
	}

	return s.validateErr
}

func TestTavilyValidateAuth(t *testing.T) {
	t.Parallel()

	var probeBody string

	t.Run("success and probe shape", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw, _ := io.ReadAll(r.Body)
			probeBody = string(raw)
			_, _ = io.WriteString(w, `{"query":"test","results":[]}`)
		}))
		defer server.Close()

		searcher := NewTavilySearcher(server.Client(), "tavily-token", testLogger())
		searcher.searchURL = server.URL

		if err := searcher.ValidateAuth(context.Background()); err != nil {
			t.Fatalf("ValidateAuth() error = %v", err)
		}
		if !strings.Contains(probeBody, `"max_results":1`) {
			t.Fatalf("probe must request exactly one result, got %s", probeBody)
		}
	})

	t.Run("definitive and transient classifications", func(t *testing.T) {
		cases := []struct {
			status int
			body   string
			kind   provider.ErrorKind
		}{
			{http.StatusUnauthorized, `{"error":"Invalid API key"}`, provider.ErrorKindAuthFailed},
			{http.StatusPaymentRequired, `{"message":"Insufficient balance"}`, provider.ErrorKindCreditsExhausted},
			{http.StatusTooManyRequests, `{}`, provider.ErrorKindRateLimited},
			// The body wording must not upgrade a transient status: a
			// per-minute quota message rides an ordinary rate limit, and
			// startup validation treats credits-exhausted as fatal.
			{http.StatusTooManyRequests, `{"message":"You have exceeded your quota of requests per minute"}`, provider.ErrorKindRateLimited},
			// Canonical statuses classify before any body wording.
			{http.StatusPaymentRequired, `{"error":"quota exceeded, retry later"}`, provider.ErrorKindCreditsExhausted},
			// An upstream failure is transient regardless of wording: an
			// outage body carrying credential-adjacent words must never
			// abort startup as exhausted credits.
			{http.StatusServiceUnavailable, `{"detail":"Credit accounting service temporarily unavailable"}`, provider.ErrorKindUpstreamUnavailable},
			{http.StatusInternalServerError, `{"detail":"quota accounting is degraded"}`, provider.ErrorKindUpstreamUnavailable},
			{http.StatusInternalServerError, `{}`, provider.ErrorKindUpstreamUnavailable},
		}

		for _, testCase := range cases {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(testCase.status)
				_, _ = io.WriteString(w, testCase.body)
			}))

			searcher := NewTavilySearcher(server.Client(), "tavily-token", testLogger())
			searcher.searchURL = server.URL

			err := searcher.ValidateAuth(context.Background())
			server.Close()
			if kind := provider.KindOf(err); kind != testCase.kind {
				t.Fatalf("status %d: unexpected error kind: %s", testCase.status, kind)
			}
		}
	})
}

func TestKagiValidateAuth(t *testing.T) {
	t.Parallel()

	t.Run("success and probe shape", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if got := r.URL.Query().Get("limit"); got != "1" {
				t.Fatalf("probe must request limit=1, got %q", got)
			}
			_, _ = io.WriteString(w, `{"meta":{"api_balance":1},"data":[]}`)
		}))
		defer server.Close()

		searcher := NewKagiSearcher(server.Client(), "kagi-token", testLogger())
		searcher.searchURL = server.URL

		if err := searcher.ValidateAuth(context.Background()); err != nil {
			t.Fatalf("ValidateAuth() error = %v", err)
		}
	})

	t.Run("definitive and transient classifications", func(t *testing.T) {
		cases := []struct {
			status int
			body   string
			kind   provider.ErrorKind
		}{
			{http.StatusForbidden, `{"data":null,"error":[{"code":2,"msg":"Invalid key"}]}`, provider.ErrorKindAuthFailed},
			{http.StatusForbidden, `{"data":null,"error":[{"code":100,"msg":"Insufficient credit"}]}`, provider.ErrorKindCreditsExhausted},
			// An upstream failure is transient regardless of body content:
			// stale in-body error codes and outage wording ("billing
			// service unavailable") must never abort startup as exhausted
			// credits.
			{http.StatusServiceUnavailable, `{"data":null,"error":[{"code":100,"msg":"Insufficient credit"}]}`, provider.ErrorKindUpstreamUnavailable},
			{http.StatusServiceUnavailable, `{"detail":"billing service temporarily unavailable"}`, provider.ErrorKindUpstreamUnavailable},
			{http.StatusInternalServerError, `{}`, provider.ErrorKindUpstreamUnavailable},
		}

		for _, testCase := range cases {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(testCase.status)
				_, _ = io.WriteString(w, testCase.body)
			}))

			searcher := NewKagiSearcher(server.Client(), "kagi-token", testLogger())
			searcher.searchURL = server.URL

			err := searcher.ValidateAuth(context.Background())
			server.Close()
			if kind := provider.KindOf(err); kind != testCase.kind {
				t.Fatalf("status %d: unexpected error kind: %s", testCase.status, kind)
			}
		}
	})
}

func TestChainValidateAuthAggregatesDefinitiveOnly(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))

	chain := &ChainSearcher{
		searchers: []namedSearcher{
			// Definitive: must be joined into the returned error.
			{name: "kagi", searcher: stubAuthSearcher{validateErr: provider.NewError("kagi", provider.ErrorKindAuthFailed)}},
			// Transient: logged, not part of the error.
			{name: "tavily", searcher: stubAuthSearcher{validateErr: provider.NewError("tavily", provider.ErrorKindRateLimited)}},
			// No validator: skipped entirely.
			{name: "stub", searcher: stubSearcher{}},
		},
		logger: logger,
	}

	err := chain.ValidateAuth(context.Background())
	if err == nil {
		t.Fatal("expected definitive rejection to abort")
	}
	if !strings.Contains(err.Error(), "kagi: auth_failed") {
		t.Fatalf("error must name the definitive provider, got: %v", err)
	}
	if strings.Contains(err.Error(), "tavily") {
		t.Fatalf("transient failure must not abort, got: %v", err)
	}
	if !strings.Contains(logs.String(), "search provider auth validation failed") || !strings.Contains(logs.String(), "provider=tavily") {
		t.Fatalf("transient failure must be logged, got: %s", logs.String())
	}

	allHealthy := &ChainSearcher{
		searchers: []namedSearcher{
			{name: "kagi", searcher: stubAuthSearcher{}},
			{name: "tavily", searcher: stubAuthSearcher{}},
		},
		logger: logger,
	}
	if err := allHealthy.ValidateAuth(context.Background()); err != nil {
		t.Fatalf("healthy chain must validate cleanly: %v", err)
	}
}

// TestChainValidateAuthHungMemberDoesNotStarveSuccessors pins the concurrent
// probe fix: chain members share the caller's deadline, so a hung first
// member must not consume the budget of the rest. Probed sequentially, its
// timeout hands the successor an already-expired context; the definitive
// rejection then arrives as a transient timeout and an invalid key boots.
func TestChainValidateAuthHungMemberDoesNotStarveSuccessors(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	hung := stubAuthSearcher{validate: func(ctx context.Context) error {
		<-release

		return ctx.Err() // the caller's deadline expired while we were blocked
	}}
	definitive := stubAuthSearcher{validate: func(ctx context.Context) error {
		if err := ctx.Err(); err != nil {
			return err // a real probe cannot answer on an expired context
		}

		return provider.NewError("kagi", provider.ErrorKindAuthFailed)
	}}

	chain := &ChainSearcher{
		searchers: []namedSearcher{
			{name: "tavily", searcher: hung},
			{name: "kagi", searcher: definitive},
		},
		logger: testLogger(),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	result := make(chan error, 1)
	go func() { result <- chain.ValidateAuth(ctx) }()

	// Let the caller's deadline expire while the first member is still
	// blocked, then let it return.
	time.Sleep(200 * time.Millisecond)
	close(release)

	err := <-result
	if !provider.IsDefinitiveRejection(err) {
		t.Fatalf("the hung member must not starve the definitive rejection: %v", err)
	}
	if !strings.Contains(err.Error(), "kagi") {
		t.Fatalf("error must name the definitive provider: %v", err)
	}
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
