package bot

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"telegram-ollama-reply-bot/internal/llm"
	"telegram-ollama-reply-bot/internal/state"
	"telegram-ollama-reply-bot/internal/vision"

	tg "github.com/mymmrac/telego"
)

// stubStats records usage accounting calls.
type stubStats struct {
	mu                 sync.Mutex
	addUsageCalls      int
	lastPromptTokens   int
	lastCompletionToks int
	lastTotalTokens    int
	lastCost           float64
}

func (s *stubStats) Snapshot() state.StatsSnapshot { return state.StatsSnapshot{} }
func (s *stubStats) String() string                { return "" }
func (s *stubStats) InlineQuery()                  {}
func (s *stubStats) GroupRequest()                 {}
func (s *stubStats) PrivateRequest()               {}
func (s *stubStats) Mention()                      {}
func (s *stubStats) SummarizeRequest()             {}
func (s *stubStats) ChatHistoryReset()             {}
func (s *stubStats) LlmTimeout()                   {}

func (s *stubStats) AddUsage(prompt, completion, total int, cost float64) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.addUsageCalls++
	s.lastPromptTokens = prompt
	s.lastCompletionToks = completion
	s.lastTotalTokens = total
	s.lastCost = cost
}

// fakeDescriber fakes the vision service for the bot.
type fakeDescriber struct {
	recognition vision.Recognition
	describeErr error
	calls       int
	lastBytes   []byte

	analyzeCalls   int
	lastAnalyzeArg analyzeCall
}

// analyzeCall records what the Analyze path received.
type analyzeCall struct {
	bytes  []byte
	prompt string
}

func (d *fakeDescriber) Describe(_ context.Context, imageData []byte) (vision.Recognition, error) {
	d.calls++
	d.lastBytes = imageData

	return d.recognition, d.describeErr
}

func (d *fakeDescriber) Analyze(_ context.Context, imageData []byte, prompt string) (vision.Recognition, error) {
	d.analyzeCalls++
	d.lastAnalyzeArg = analyzeCall{bytes: imageData, prompt: prompt}

	return d.recognition, d.describeErr
}

// newImageTestBot wires a Bot to a fake Bot API server that serves getFile
// and file downloads, and counts requests. The returned file body is what
// /file/bot.../ serves.
func newImageTestBot(t *testing.T, fileBody []byte) (*Bot, *fakeDescriber, *stubStats, *atomic.Int32) {
	t.Helper()

	var requests atomic.Int32

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)

		switch {
		case strings.HasSuffix(r.URL.Path, "/getFile"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"ok":true,"result":{"file_id":"f","file_unique_id":"u","file_path":"photos/file_1.jpg"}}`)
		case strings.HasPrefix(r.URL.Path, "/file/bot"):
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write(fileBody)
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"ok":true,"result":{}}`)
		}
	}))
	t.Cleanup(ts.Close)

	api, err := tg.NewBot("1:"+strings.Repeat("A", 35), tg.WithAPIServer(ts.URL), tg.WithDiscardLogger())
	if err != nil {
		t.Fatalf("cannot create test bot api: %v", err)
	}

	describer := &fakeDescriber{}
	stats := &stubStats{}

	b := &Bot{
		api:    api,
		vision: describer,
		stats:  stats,
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		ctx:    context.Background(),
	}

	return b, describer, stats, &requests
}

var jpegTestBody = append([]byte{0xFF, 0xD8, 0xFF}, []byte("jpeg bytes")...)

func TestDescribeImage_AccountsUsage(t *testing.T) {
	t.Parallel()

	b, describer, stats, _ := newImageTestBot(t, jpegTestBody)
	b.imageRecognition = ImageRecognitionSettings{Enabled: true, MaxImageBytes: 1 << 20}
	describer.recognition = vision.Recognition{
		Text:  "a chart",
		Usage: &llm.TokenUsage{PromptTokens: 11, CompletionTokens: 7, TotalTokens: 18, Cost: 0.5},
	}

	description, err := b.describeImage(context.Background(), &state.ImageMeta{FileID: "f", FileSize: len(jpegTestBody)})
	if err != nil {
		t.Fatalf("describeImage: %v", err)
	}

	if description != "a chart" {
		t.Fatalf("unexpected description: %q", description)
	}
	if describer.calls != 1 {
		t.Fatalf("expected one describe call, got %d", describer.calls)
	}
	if !strings.HasPrefix(string(describer.lastBytes), "\xff\xd8\xff") {
		t.Fatalf("expected downloaded bytes passed to vision")
	}

	stats.mu.Lock()
	defer stats.mu.Unlock()

	if stats.addUsageCalls != 1 {
		t.Fatalf("expected one AddUsage call, got %d", stats.addUsageCalls)
	}
	if stats.lastPromptTokens != 11 || stats.lastCompletionToks != 7 || stats.lastTotalTokens != 18 || stats.lastCost != 0.5 {
		t.Fatalf("unexpected usage recorded: %+v", stats)
	}
}

func TestDescribeImage_NilUsageSkipsAccounting(t *testing.T) {
	t.Parallel()

	b, describer, stats, _ := newImageTestBot(t, jpegTestBody)
	b.imageRecognition = ImageRecognitionSettings{Enabled: true, MaxImageBytes: 1 << 20}
	describer.recognition = vision.Recognition{Text: "model-free route"}

	if _, err := b.describeImage(context.Background(), &state.ImageMeta{FileID: "f", FileSize: len(jpegTestBody)}); err != nil {
		t.Fatalf("describeImage: %v", err)
	}

	stats.mu.Lock()
	defer stats.mu.Unlock()

	if stats.addUsageCalls != 0 {
		t.Fatalf("model-free routes must not account usage, got %d calls", stats.addUsageCalls)
	}
}

func TestDescribeImage_FileSizePreCheckSkipsDownload(t *testing.T) {
	t.Parallel()

	b, describer, _, requests := newImageTestBot(t, jpegTestBody)
	b.imageRecognition = ImageRecognitionSettings{Enabled: true, MaxImageBytes: 8}

	_, err := b.describeImage(context.Background(), &state.ImageMeta{FileID: "f", FileSize: 100})
	if !errors.Is(err, ErrImageRecognition) || !errors.Is(err, vision.ErrImageTooLarge) {
		t.Fatalf("expected joined size-bound error, got %v", err)
	}

	if got := requests.Load(); got != 0 {
		t.Fatalf("oversized file must not be downloaded, got %d requests", got)
	}
	if describer.calls != 0 {
		t.Fatalf("oversized file must not reach the recognizer")
	}
}

func TestDescribeImage_RecognizerErrorJoinsSentinel(t *testing.T) {
	t.Parallel()

	b, describer, _, _ := newImageTestBot(t, jpegTestBody)
	b.imageRecognition = ImageRecognitionSettings{Enabled: true, MaxImageBytes: 1 << 20}
	describer.describeErr = vision.ErrRecognizerUnavailable

	_, err := b.describeImage(context.Background(), &state.ImageMeta{FileID: "f", FileSize: 10})
	if !errors.Is(err, ErrImageRecognition) || !errors.Is(err, vision.ErrRecognizerUnavailable) {
		t.Fatalf("expected joined recognizer error, got %v", err)
	}
}

func TestDownloadFileWithContext_RespectsBound(t *testing.T) {
	t.Parallel()

	body := []byte(strings.Repeat("x", 100))
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	}))
	t.Cleanup(ts.Close)

	data, err := downloadFileWithContext(context.Background(), ts.URL, 128)
	if err != nil {
		t.Fatalf("download within bound: %v", err)
	}
	if string(data) != string(body) {
		t.Fatalf("unexpected download payload")
	}

	_, err = downloadFileWithContext(context.Background(), ts.URL, 10)
	if err == nil {
		t.Fatalf("expected bound error")
	}
	if !strings.Contains(err.Error(), "exceeds bound") {
		t.Fatalf("unexpected bound error: %v", err)
	}
}

// TestDownloadFileWithContext_TransportErrorSanitized ensures the bot token
// embedded in the Telegram file URL never reaches the returned error chain.
func TestDownloadFileWithContext_TransportErrorSanitized(t *testing.T) {
	t.Parallel()

	const sentinel = "TESTSECRET"

	_, err := downloadFileWithContext(context.Background(), "http://127.0.0.1:1/file/bot"+sentinel+"/photo.jpg", 128)
	if err == nil {
		t.Fatalf("expected transport error")
	}
	if strings.Contains(err.Error(), sentinel) {
		t.Fatalf("download error leaks the token-bearing URL: %q", err.Error())
	}
}

// TestAnalyzeImage_AccountsUsageAndNeverTouchesCache is the model-prompted
// path: the prompt reaches the vision layer verbatim, usage is accounted, and
// the baseline description cache (and any stored message) stays untouched.
func TestAnalyzeImage_AccountsUsageAndNeverTouchesCache(t *testing.T) {
	t.Parallel()

	b, describer, stats, _ := newImageTestBot(t, jpegTestBody)
	b.imageRecognition = ImageRecognitionSettings{Enabled: true, MaxImageBytes: 1 << 20}
	cache := &mapImageCache{entries: map[string]string{}}
	b.imageCache = cache
	describer.recognition = vision.Recognition{
		Text:  "focused analysis",
		Usage: &llm.TokenUsage{PromptTokens: 3, CompletionTokens: 5, TotalTokens: 8, Cost: 0.25},
	}

	analysis, err := b.AnalyzeImage(context.Background(), state.ImageMeta{FileID: "f", FileSize: len(jpegTestBody)}, "read the error text")
	if err != nil {
		t.Fatalf("AnalyzeImage: %v", err)
	}

	if analysis != "focused analysis" {
		t.Fatalf("unexpected analysis: %q", analysis)
	}
	if describer.analyzeCalls != 1 || describer.calls != 0 {
		t.Fatalf("expected exactly one Analyze call and no Describe calls, got analyze=%d describe=%d", describer.analyzeCalls, describer.calls)
	}
	if describer.lastAnalyzeArg.prompt != "read the error text" {
		t.Fatalf("expected prompt forwarded verbatim, got %q", describer.lastAnalyzeArg.prompt)
	}
	if !strings.HasPrefix(string(describer.lastAnalyzeArg.bytes), "\xff\xd8\xff") {
		t.Fatalf("expected downloaded bytes passed to vision")
	}

	if len(cache.entries) != 0 {
		t.Fatalf("analysis must never write the description cache, got %v", cache.entries)
	}

	stats.mu.Lock()
	defer stats.mu.Unlock()

	if stats.addUsageCalls != 1 || stats.lastTotalTokens != 8 || stats.lastCost != 0.25 {
		t.Fatalf("unexpected usage accounting: %+v", stats)
	}
}

// TestAnalyzeImage_BlankPromptRefused asserts the vision-layer blank-prompt
// guard surfaces through the bot with no fetch work performed.
func TestAnalyzeImage_BlankPromptRefused(t *testing.T) {
	t.Parallel()

	b, describer, _, requests := newImageTestBot(t, jpegTestBody)
	b.imageRecognition = ImageRecognitionSettings{Enabled: true, MaxImageBytes: 1 << 20}

	_, err := b.AnalyzeImage(context.Background(), state.ImageMeta{FileID: "f", FileSize: 10}, "   ")
	if !errors.Is(err, ErrImageRecognition) || !errors.Is(err, vision.ErrPromptEmpty) {
		t.Fatalf("expected joined ErrPromptEmpty, got %v", err)
	}
	if describer.analyzeCalls != 0 || requests.Load() != 0 {
		t.Fatalf("blank prompt must refuse before recognition (calls=%d requests=%d)", describer.analyzeCalls, requests.Load())
	}
}

func TestAnalyzeImage_NilUsageSkipsAccounting(t *testing.T) {
	t.Parallel()

	b, describer, stats, _ := newImageTestBot(t, jpegTestBody)
	b.imageRecognition = ImageRecognitionSettings{Enabled: true, MaxImageBytes: 1 << 20}
	describer.recognition = vision.Recognition{Text: "model-free analysis"}

	if _, err := b.AnalyzeImage(context.Background(), state.ImageMeta{FileID: "f", FileSize: len(jpegTestBody)}, "look"); err != nil {
		t.Fatalf("AnalyzeImage: %v", err)
	}

	stats.mu.Lock()
	defer stats.mu.Unlock()

	if stats.addUsageCalls != 0 {
		t.Fatalf("model-free analysis must not account usage, got %d calls", stats.addUsageCalls)
	}
}

func TestAnalyzeImage_DisabledRefusesWithoutCalls(t *testing.T) {
	t.Parallel()

	b, describer, _, requests := newImageTestBot(t, jpegTestBody)
	b.imageRecognition = ImageRecognitionSettings{Enabled: false, MaxImageBytes: 1 << 20}

	_, err := b.AnalyzeImage(context.Background(), state.ImageMeta{FileID: "f", FileSize: 10}, "look")
	if !errors.Is(err, ErrImageRecognition) {
		t.Fatalf("expected ErrImageRecognition when disabled, got %v", err)
	}
	if describer.analyzeCalls != 0 || requests.Load() != 0 {
		t.Fatalf("disabled analyzer must refuse before any work (calls=%d requests=%d)", describer.analyzeCalls, requests.Load())
	}
}

func TestAnalyzeImage_FileSizePreCheckSkipsDownload(t *testing.T) {
	t.Parallel()

	b, describer, _, requests := newImageTestBot(t, jpegTestBody)
	b.imageRecognition = ImageRecognitionSettings{Enabled: true, MaxImageBytes: 8}

	_, err := b.AnalyzeImage(context.Background(), state.ImageMeta{FileID: "f", FileSize: 100}, "look")
	if !errors.Is(err, ErrImageRecognition) || !errors.Is(err, vision.ErrImageTooLarge) {
		t.Fatalf("expected joined size-bound error, got %v", err)
	}
	if requests.Load() != 0 || describer.analyzeCalls != 0 {
		t.Fatalf("oversized file must not be fetched (requests=%d analyze=%d)", requests.Load(), describer.analyzeCalls)
	}
}

// TestAnalyzeImage_DownloadOversizeKeepsSizeKind covers the streamed-body
// bound when the metadata is zero or underreported: the vision size sentinel
// must survive so callers classify the failure as image_too_large.
func TestAnalyzeImage_DownloadOversizeKeepsSizeKind(t *testing.T) {
	t.Parallel()

	for name, reportedSize := range map[string]int{
		"zero metadata":      0,
		"underreported size": 4,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			b, describer, _, requests := newImageTestBot(t, []byte(strings.Repeat("x", 100)))
			b.imageRecognition = ImageRecognitionSettings{Enabled: true, MaxImageBytes: 10}

			_, err := b.AnalyzeImage(context.Background(), state.ImageMeta{FileID: "f", FileSize: reportedSize}, "read")
			if !errors.Is(err, vision.ErrImageTooLarge) {
				t.Fatalf("download-time oversize must keep the size kind, got %v", err)
			}
			if !errors.Is(err, ErrImageRecognition) || !errors.Is(err, errFileDownload) {
				t.Fatalf("expected joined sentinels preserved, got %v", err)
			}
			if requests.Load() == 0 || describer.analyzeCalls != 0 {
				t.Fatalf("expected one bounded download and no recognition (requests=%d analyze=%d)", requests.Load(), describer.analyzeCalls)
			}
		})
	}
}

// TestAnalyzeImage_GetFileErrorSanitized reuses the #113 guarantee on the
// analysis path: a getFile transport error carries the token-bearing URL and
// must be classified and discarded before the error escapes.
func TestAnalyzeImage_GetFileErrorSanitized(t *testing.T) {
	t.Parallel()

	token := "1:" + strings.Repeat("A", 35)
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return nil, errors.New("metadata transport failed at " + r.URL.String())
	})}
	api, err := tg.NewBot(token, tg.WithHTTPClient(client), tg.WithDiscardLogger())
	if err != nil {
		t.Fatalf("cannot create test bot api: %v", err)
	}

	b := &Bot{
		api:              api,
		logger:           slog.New(slog.NewTextHandler(io.Discard, nil)),
		ctx:              context.Background(),
		vision:           &fakeDescriber{},
		stats:            &stubStats{},
		imageRecognition: ImageRecognitionSettings{Enabled: true, MaxImageBytes: 1 << 20},
	}

	_, err = b.AnalyzeImage(context.Background(), state.ImageMeta{FileID: "f", FileSize: 10}, "look")
	if err == nil {
		t.Fatalf("expected metadata error")
	}
	if !errors.Is(err, ErrImageRecognition) {
		t.Fatalf("expected ErrImageRecognition joined, got %v", err)
	}
	assertImageErrorTreeClean(t, err, token)
}

// roundTripFunc adapts a function to http.RoundTripper.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// countingReader counts the bytes actually read from the underlying reader.
type countingReader struct {
	r io.Reader
	n int
}

func (r *countingReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	r.n += n

	return n, err
}

// TestDownloadFileWithContext_ErrorBodyNotDrained ensures a non-200 response
// body is never consumed unboundedly: an endless error body must neither
// bypass the size bound nor stall the caller before the HTTP error surfaces.
//
// Deliberately NOT parallel: it swaps the package-global fileDownloadClient,
// which parallel package tests read. Sequential tests run one at a time while
// parallel tests are paused, so the mutation is isolated; the cleanup still
// restores the original client.
func TestDownloadFileWithContext_ErrorBodyNotDrained(t *testing.T) {
	body := &countingReader{r: strings.NewReader(strings.Repeat("x", 100_000))}
	old := fileDownloadClient
	fileDownloadClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusInternalServerError, Body: io.NopCloser(body)}, nil
	})}
	t.Cleanup(func() { fileDownloadClient = old })

	_, err := downloadFileWithContext(context.Background(), "https://example.test/file", 128)
	if err == nil {
		t.Fatalf("expected HTTP error")
	}
	if !strings.Contains(err.Error(), "http status 500") {
		t.Fatalf("expected numeric HTTP status in error, got %v", err)
	}
	if body.n > 129 {
		t.Fatalf("error response consumed %d bytes despite the 128-byte bound", body.n)
	}
}

// mapImageCache is an in-memory state.ImageStore stub.
type mapImageCache struct {
	entries map[string]string
}

func (c *mapImageCache) Get(meta *state.ImageMeta) (string, bool) {
	desc, ok := c.entries[meta.FileID]

	return desc, ok
}

func (c *mapImageCache) Set(meta *state.ImageMeta, description string) {
	c.entries[meta.FileID] = description
}

// assertImageErrorTreeClean recursively walks the error tree (single and
// multi Unwrap — what Sentry's exception chain traverses) and fails when any
// node's message contains one of the sentinels.
func assertImageErrorTreeClean(t *testing.T, err error, sentinels ...string) {
	t.Helper()

	seen := make(map[error]bool)

	var visit func(error)
	visit = func(e error) {
		if e == nil || seen[e] {
			return
		}
		seen[e] = true

		msg := e.Error()
		for _, s := range sentinels {
			if strings.Contains(msg, s) {
				t.Fatalf("error tree leaks %q: %q", s, msg)
			}
		}

		//nolint:errorlint // intentional: enumerating the direct unwrap structure like Sentry, not matching a target type
		switch unwrapper := e.(type) {
		case interface{ Unwrap() error }:
			visit(unwrapper.Unwrap())
		case interface{ Unwrap() []error }:
			for _, child := range unwrapper.Unwrap() {
				visit(child)
			}
		}
	}

	visit(err)
}

// TestEnsureMessageImageDescription_GetFileErrorSanitized drives the full
// logging path: a getFile transport error carries the token-bearing request
// URL, and the joined error is logged and captured — so the raw cause must be
// classified and discarded before joining, keeping only the typed kind and
// ctx sentinels.
func TestEnsureMessageImageDescription_GetFileErrorSanitized(t *testing.T) {
	t.Parallel()

	token := "1:" + strings.Repeat("A", 35)
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return nil, errors.New("metadata transport failed at " + r.URL.String())
	})}
	api, err := tg.NewBot(token, tg.WithHTTPClient(client), tg.WithDiscardLogger())
	if err != nil {
		t.Fatalf("cannot create test bot api: %v", err)
	}

	var logs bytes.Buffer
	b := &Bot{
		api:    api,
		logger: slog.New(slog.NewTextHandler(&logs, nil)),
		ctx:    context.Background(),
		vision: &fakeDescriber{},
		stats:  &stubStats{},
		imageCache: &mapImageCache{
			entries: map[string]string{},
		},
		imageRecognition: ImageRecognitionSettings{Enabled: true, MaxImageBytes: 1 << 20},
	}

	msg := &state.Message{HasImage: true, ImageMeta: &state.ImageMeta{FileID: "f", FileSize: 10}}
	b.ensureMessageImageDescription(context.Background(), msg)

	if msg.Image != "" {
		t.Fatalf("expected no description on metadata failure")
	}
	if !strings.Contains(logs.String(), "failed to describe image") {
		t.Fatalf("expected the normal-level failure log, got %q", logs.String())
	}
	if strings.Contains(logs.String(), token) {
		t.Fatalf("bot log leaks the token-bearing getFile URL: %q", logs.String())
	}

	// Re-derive the joined error from the same path to inspect the tree.
	_, err = b.describeImage(context.Background(), msg.ImageMeta)
	if err == nil {
		t.Fatalf("expected metadata error")
	}
	if !errors.Is(err, ErrImageRecognition) {
		t.Fatalf("expected ErrImageRecognition joined, got %v", err)
	}
	if !errors.Is(err, errFileMetadata) {
		t.Fatalf("expected typed metadata kind, got %v", err)
	}
	assertImageErrorTreeClean(t, err, token)
}
