package vision

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// minimaxTestServer records requests to a fake MiniMax VLM endpoint and
// replies with the configured status code and body.
type minimaxTestServer struct {
	server *httptest.Server

	mu           sync.Mutex
	requestCount int
	lastRequest  struct {
		method string
		path   string
		auth   string
		body   map[string]any
	}
}

func newMinimaxTestServer(t *testing.T, statusCode int, body string) *minimaxTestServer {
	t.Helper()

	fake := &minimaxTestServer{}
	fake.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)

		var decoded map[string]any
		_ = json.Unmarshal(raw, &decoded)

		fake.mu.Lock()
		fake.requestCount++
		fake.lastRequest.method = r.Method
		fake.lastRequest.path = r.URL.Path
		fake.lastRequest.auth = r.Header.Get("Authorization")
		fake.lastRequest.body = decoded
		fake.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(statusCode)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(fake.server.Close)

	return fake
}

func (s *minimaxTestServer) requests() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.requestCount
}

func newTestMiniMax(t *testing.T, baseURL string) *MiniMaxRecognizer {
	t.Helper()

	recognizer, err := NewMiniMaxRecognizer(baseURL, "test-key", 5*time.Second, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("new recognizer: %v", err)
	}

	return recognizer
}

func TestMiniMaxRecognize_Success(t *testing.T) {
	t.Parallel()

	fake := newMinimaxTestServer(t, http.StatusOK, `{"content":"a sleeping cat","base_resp":{"status_code":0,"status_msg":"success"}}`)
	recognizer := newTestMiniMax(t, fake.server.URL)

	image := pngImage()
	recognition, err := recognizer.Recognize(context.Background(), image, "describe the image")
	if err != nil {
		t.Fatalf("recognize: %v", err)
	}

	if recognition.Text != "a sleeping cat" {
		t.Fatalf("unexpected content: %q", recognition.Text)
	}
	if recognition.Usage != nil {
		t.Fatalf("minimax route must not fabricate usage, got %+v", recognition.Usage)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()

	if fake.lastRequest.method != http.MethodPost {
		t.Fatalf("expected POST, got %s", fake.lastRequest.method)
	}
	if fake.lastRequest.path != "/v1/coding_plan/vlm" {
		t.Fatalf("expected /v1/coding_plan/vlm, got %s", fake.lastRequest.path)
	}
	if fake.lastRequest.auth != "Bearer test-key" {
		t.Fatalf("expected bearer auth, got %q", fake.lastRequest.auth)
	}
	if fake.lastRequest.body["prompt"] != "describe the image" {
		t.Fatalf("expected prompt in body, got %v", fake.lastRequest.body["prompt"])
	}
	if _, hasModel := fake.lastRequest.body["model"]; hasModel {
		t.Fatalf("minimax vlm request must not carry a model field: %v", fake.lastRequest.body)
	}

	imageURL, _ := fake.lastRequest.body["image_url"].(string)
	const prefix = "data:image/png;base64,"
	if !strings.HasPrefix(imageURL, prefix) {
		t.Fatalf("expected %s data url, got %q", prefix, imageURL)
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(imageURL, prefix))
	if err != nil {
		t.Fatalf("data url base64 does not decode: %v", err)
	}
	if string(decoded) != string(image.Data) {
		t.Fatalf("decoded image bytes differ from input")
	}
}

func TestMiniMaxRecognize_HTTPAndProviderStatuses(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		statusCode int
		body       string
		wantErr    error
		wantText   string
	}{
		{
			name:       "http 500",
			statusCode: http.StatusInternalServerError,
			body:       `{"base_resp":{"status_code":500,"status_msg":"boom"}}`,
			wantErr:    ErrRecognizerUnavailable,
			wantText:   "500",
		},
		{
			name:       "provider auth status 1004",
			statusCode: http.StatusOK,
			body:       `{"base_resp":{"status_code":1004,"status_msg":"invalid api key"}}`,
			wantErr:    ErrRecognizerAuth,
			wantText:   "1004",
		},
		{
			name:       "provider status 2038",
			statusCode: http.StatusOK,
			body:       `{"base_resp":{"status_code":2038,"status_msg":"realname verification"}}`,
			wantErr:    ErrRecognizerStatus,
			wantText:   "2038",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			fake := newMinimaxTestServer(t, tc.statusCode, tc.body)
			recognizer := newTestMiniMax(t, fake.server.URL)

			_, err := recognizer.Recognize(context.Background(), pngImage(), "describe")
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("expected %v, got %v", tc.wantErr, err)
			}
			if !strings.Contains(err.Error(), tc.wantText) {
				t.Fatalf("expected %s in error, got %q", tc.wantText, err.Error())
			}
		})
	}
}

// TestMiniMaxRecognize_RejectsMalformedResponses is the presence/type matrix:
// a missing base_resp, a missing/null/non-integer status_code, or an
// undecodable body must all fail — none may default status_code to zero and
// succeed.
func TestMiniMaxRecognize_RejectsMalformedResponses(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		body    string
		wantErr error
	}{
		{"missing base_resp", `{"content":"x"}`, ErrRecognizerMalformed},
		{"empty base_resp object", `{"content":"x","base_resp":{}}`, ErrRecognizerMalformed},
		{"null base_resp", `{"content":"x","base_resp":null}`, ErrRecognizerMalformed},
		{"base_resp array", `{"content":"x","base_resp":[]}`, ErrRecognizerMalformed},
		{"null status_code", `{"content":"x","base_resp":{"status_code":null}}`, ErrRecognizerMalformed},
		{"string status_code", `{"content":"x","base_resp":{"status_code":"0"}}`, ErrRecognizerMalformed},
		{"fractional status_code", `{"content":"x","base_resp":{"status_code":0.5}}`, ErrRecognizerMalformed},
		{"boolean status_code", `{"content":"x","base_resp":{"status_code":true}}`, ErrRecognizerMalformed},
		{"malformed json body", `{not json`, ErrRecognizerMalformed},
		{"missing content", `{"base_resp":{"status_code":0}}`, ErrRecognizerEmpty},
		{"null content", `{"content":null,"base_resp":{"status_code":0}}`, ErrRecognizerEmpty},
		{"non-string content", `{"content":42,"base_resp":{"status_code":0}}`, ErrRecognizerMalformed},
		{"whitespace content", `{"content":"   \n\t ","base_resp":{"status_code":0}}`, ErrRecognizerEmpty},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			fake := newMinimaxTestServer(t, http.StatusOK, tc.body)
			recognizer := newTestMiniMax(t, fake.server.URL)

			recognition, err := recognizer.Recognize(context.Background(), pngImage(), "describe")
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("expected %v, got %v (recognition=%q)", tc.wantErr, err, recognition.Text)
			}
			if recognition.Text != "" {
				t.Fatalf("no malformed response may produce content, got %q", recognition.Text)
			}
		})
	}
}

func TestMiniMaxRecognize_RejectsResponseOverCap(t *testing.T) {
	t.Parallel()

	fake := newMinimaxTestServer(t, http.StatusOK, `{"content":"`+strings.Repeat("a", maxResponseBytes+2)+`","base_resp":{"status_code":0}}`)
	recognizer := newTestMiniMax(t, fake.server.URL)

	_, err := recognizer.Recognize(context.Background(), pngImage(), "describe")
	if !errors.Is(err, ErrRecognizerMalformed) {
		t.Fatalf("expected malformed error for oversized response, got %v", err)
	}
}

func TestMiniMaxRecognize_RejectsEmptyPromptBeforeRequest(t *testing.T) {
	t.Parallel()

	fake := newMinimaxTestServer(t, http.StatusOK, `{"content":"x","base_resp":{"status_code":0}}`)
	recognizer := newTestMiniMax(t, fake.server.URL)

	_, err := recognizer.Recognize(context.Background(), pngImage(), " ")
	if !errors.Is(err, ErrPromptEmpty) {
		t.Fatalf("expected ErrPromptEmpty, got %v", err)
	}
	if fake.requests() != 0 {
		t.Fatalf("expected zero requests, got %d", fake.requests())
	}
}

func TestMiniMaxRecognize_ContextCancelPropagates(t *testing.T) {
	t.Parallel()

	// The handler drains the body (so the server starts watching for the
	// client disconnect) and then blocks until the request context is done —
	// the only way Recognize returns is through client-side cancellation.
	ts := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		<-r.Context().Done()
	}))
	t.Cleanup(ts.Close)

	recognizer := newTestMiniMax(t, ts.URL)

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, err := recognizer.Recognize(ctx, pngImage(), "describe")
		errCh <- err
	}()

	// Give the request time to reach the blocked handler, then cancel.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled to stay matchable, got %v", err)
		}
		if !errors.Is(err, ErrRecognizerUnavailable) {
			t.Fatalf("expected typed kind, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("recognize did not return after cancellation")
	}
}

// TestMiniMaxRecognize_NoSecretsInErrorsOrChains is the Sentry-grade leak
// regression: sentinels injected into provider status messages, response
// content, and the transport URL must appear in neither the error text at any
// reachable chain node nor the captured log output.
func TestMiniMaxRecognize_NoSecretsInErrorsOrChains(t *testing.T) {
	t.Parallel()

	const (
		secretStatusMsg = "leak sk-TESTSECRET" //nolint:gosec // test fixture: sentinel that must never reach errors or logs
		secretContent   = "TESTSECRET-2"
		secretURLPart   = "TESTSECRET-3"
	)

	t.Run("provider status message", func(t *testing.T) {
		t.Parallel()

		var logBuf lockedBuffer
		fake := newMinimaxTestServer(t, http.StatusInternalServerError,
			fmt.Sprintf(`{"base_resp":{"status_code":999,"status_msg":%q}}`, secretStatusMsg))
		recognizer, err := NewMiniMaxRecognizer(fake.server.URL, "test-key", 5*time.Second, slog.New(slog.NewTextHandler(&logBuf, nil)))
		if err != nil {
			t.Fatalf("new recognizer: %v", err)
		}

		_, recErr := recognizer.Recognize(context.Background(), pngImage(), "describe")
		if recErr == nil {
			t.Fatalf("expected error")
		}

		assertErrorChainClean(t, recErr, secretStatusMsg, secretContent, secretURLPart)
		if logText := logBuf.String(); strings.Contains(logText, secretStatusMsg) {
			t.Fatalf("log leaks provider status message: %q", logText)
		}
		// The safe parts survive: the numeric HTTP status.
		if !strings.Contains(recErr.Error(), "500") {
			t.Fatalf("expected http status in error, got %q", recErr.Error())
		}
	})

	t.Run("response content on empty result", func(t *testing.T) {
		t.Parallel()

		fake := newMinimaxTestServer(t, http.StatusOK,
			fmt.Sprintf(`{"content":"","base_resp":{"status_code":0,"status_msg":%q}}`, secretContent))
		recognizer := newTestMiniMax(t, fake.server.URL)

		_, recErr := recognizer.Recognize(context.Background(), pngImage(), "describe")
		if !errors.Is(recErr, ErrRecognizerEmpty) {
			t.Fatalf("expected empty error, got %v", recErr)
		}

		assertErrorChainClean(t, recErr, secretStatusMsg, secretContent, secretURLPart)
	})

	t.Run("transport url", func(t *testing.T) {
		t.Parallel()

		// Port 1 refuses connections; the URL path carries the sentinel, and
		// the raw url.Error text would embed it verbatim.
		recognizer := newTestMiniMax(t, "http://127.0.0.1:1/"+secretURLPart)

		recErr := func() error {
			_, err := recognizer.Recognize(context.Background(), pngImage(), "describe")

			return err
		}()
		if recErr == nil {
			t.Fatalf("expected error")
		}
		if !errors.Is(recErr, ErrRecognizerUnavailable) {
			t.Fatalf("expected typed kind, got %v", recErr)
		}

		assertErrorChainClean(t, recErr, secretStatusMsg, secretContent, secretURLPart)
	})
}

func TestNewMiniMaxRecognizer_ValidatesConfig(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	cases := []struct {
		name    string
		baseURL string
	}{
		{"not a url", "not a url at all"},
		{"wrong scheme", "ftp://api.minimax.io"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := NewMiniMaxRecognizer(tc.baseURL, "key", time.Second, logger)
			if !errors.Is(err, ErrRecognizerConfigured) {
				t.Fatalf("expected ErrRecognizerConfigured, got %v", err)
			}
		})
	}

	if _, err := NewMiniMaxRecognizer("https://api.minimax.io", "key", 0, logger); !errors.Is(err, ErrRecognizerConfigured) {
		t.Fatalf("non-positive timeout must be rejected, got %v", err)
	}
}

func TestMiniMaxRecognizer_Metadata(t *testing.T) {
	t.Parallel()

	recognizer := newTestMiniMax(t, "https://api.minimax.io")

	if recognizer.Name() != "minimax_vlm" {
		t.Fatalf("unexpected name: %s", recognizer.Name())
	}
	if recognizer.RequiresModel() {
		t.Fatalf("minimax route is model-free")
	}
	if !recognizer.Supports("image/jpeg") || recognizer.Supports("image/gif") {
		t.Fatalf("unexpected Supports results")
	}
}

// lockedBuffer is an io.Writer safe for concurrent use by a slog handler.
type lockedBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.String()
}

// assertErrorChainClean recursively walks the error chain (single and multi
// Unwrap) — the same traversal Sentry's exception chain performs — and fails
// when any node's message contains one of the sentinels.
func assertErrorChainClean(t *testing.T, err error, sentinels ...string) {
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
				t.Fatalf("error chain leaks %q: %q", s, msg)
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
