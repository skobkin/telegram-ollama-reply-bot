package bot

import (
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
}

func (d *fakeDescriber) Describe(_ context.Context, imageData []byte) (vision.Recognition, error) {
	d.calls++
	d.lastBytes = imageData

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
