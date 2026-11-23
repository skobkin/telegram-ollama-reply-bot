package imagegen

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"telegram-ollama-reply-bot/internal/provider"
)

const testToken = "sk-test-secret-token"

// pngBytes returns a real, tiny PNG so MIME sniffing sees a genuine signature.
func pngBytes(t testing.TB) []byte {
	t.Helper()

	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatalf("encode png: %v", err)
	}

	return buf.Bytes()
}

func b64(data []byte) string { return base64.StdEncoding.EncodeToString(data) }

type recordedRequest struct {
	method string
	path   string
	auth   string
	ctype  string
	body   map[string]any
}

// fakeImagesServer replies with a fixed status/body and records requests.
type fakeImagesServer struct {
	server *httptest.Server

	mu       sync.Mutex
	requests []recordedRequest
}

func newFakeImagesServer(t *testing.T, status int, body string) *fakeImagesServer {
	t.Helper()

	fake := &fakeImagesServer{}
	fake.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var decoded map[string]any
		_ = json.Unmarshal(raw, &decoded)

		fake.mu.Lock()
		fake.requests = append(fake.requests, recordedRequest{
			method: r.Method,
			path:   r.URL.Path,
			auth:   r.Header.Get("Authorization"),
			ctype:  r.Header.Get("Content-Type"),
			body:   decoded,
		})
		fake.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(fake.server.Close)

	return fake
}

func (f *fakeImagesServer) only(t *testing.T) recordedRequest {
	t.Helper()

	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) != 1 {
		t.Fatalf("expected exactly one request, got %d", len(f.requests))
	}

	return f.requests[0]
}

func newBackend(t *testing.T, baseURL string, maxImageBytes int64) *OpenAICompat {
	t.Helper()

	backend, err := NewOpenAICompat(baseURL, testToken, http.DefaultClient, maxImageBytes, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("new backend: %v", err)
	}

	return backend
}

func successBody(t testing.TB, data []byte, revised string) string {
	t.Helper()

	raw, err := json.Marshal(map[string]any{
		"created": 1,
		"data":    []map[string]any{{"b64_json": b64(data), "revised_prompt": revised}},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	return string(raw)
}

func TestOpenAICompatGenerateSuccess(t *testing.T) {
	want := pngBytes(t)
	fake := newFakeImagesServer(t, http.StatusOK, successBody(t, want, "a revised cat"))
	backend := newBackend(t, fake.server.URL+"/v1", 0)

	got, err := backend.Generate(context.Background(), Request{Model: "MiniMax-H3-Image", Prompt: "a cat", Aspect: AspectLandscape})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if !bytes.Equal(got.Data, want) {
		t.Fatal("image bytes differ from the served image")
	}
	if got.RevisedPrompt != "a revised cat" {
		t.Fatalf("unexpected revised prompt: %q", got.RevisedPrompt)
	}

	req := fake.only(t)
	if req.method != http.MethodPost || req.path != "/v1/images/generations" {
		t.Fatalf("unexpected request line: %s %s", req.method, req.path)
	}
	if req.auth != "Bearer "+testToken {
		t.Fatalf("unexpected authorization header: %q", req.auth)
	}
	if req.ctype != "application/json" {
		t.Fatalf("unexpected content type: %q", req.ctype)
	}
	if req.body["model"] != "MiniMax-H3-Image" || req.body["prompt"] != "a cat" {
		t.Fatalf("unexpected model/prompt: %v", req.body)
	}
	if req.body["n"] != float64(1) || req.body["response_format"] != "b64_json" || req.body["size"] != "1536x1024" {
		t.Fatalf("unexpected generation parameters: %v", req.body)
	}
	for _, forbidden := range []string{"aspect_ratio", "area", "output_format", "stream"} {
		if _, present := req.body[forbidden]; present {
			t.Fatalf("provider-specific parameter %q must not be sent", forbidden)
		}
	}
}

func TestOpenAICompatMapsAspectToSize(t *testing.T) {
	cases := map[Aspect]string{
		AspectSquare:    "1024x1024",
		AspectLandscape: "1536x1024",
		AspectPortrait:  "1024x1536",
	}

	for aspect, size := range cases {
		t.Run(string(aspect), func(t *testing.T) {
			fake := newFakeImagesServer(t, http.StatusOK, successBody(t, pngBytes(t), ""))
			backend := newBackend(t, fake.server.URL+"/v1", 0)

			if _, err := backend.Generate(context.Background(), Request{Model: "m", Prompt: "p", Aspect: aspect}); err != nil {
				t.Fatalf("generate: %v", err)
			}
			if got := fake.only(t).body["size"]; got != size {
				t.Fatalf("aspect %s: expected size %s, got %v", aspect, size, got)
			}
		})
	}
}

func TestOpenAICompatRejectsUnknownAspectWithoutRequest(t *testing.T) {
	fake := newFakeImagesServer(t, http.StatusOK, successBody(t, pngBytes(t), ""))
	backend := newBackend(t, fake.server.URL+"/v1", 0)

	_, err := backend.Generate(context.Background(), Request{Model: "m", Prompt: "p", Aspect: "wide"})
	if !errors.Is(err, ErrInvalidAspect) {
		t.Fatalf("expected ErrInvalidAspect, got %v", err)
	}
	if len(fake.requests) != 0 {
		t.Fatal("no request may be sent for an invalid aspect")
	}
}

func TestOpenAICompatEndpointJoinsBaseURLPath(t *testing.T) {
	for _, suffix := range []string{"/v1", "/v1/", ""} {
		t.Run("base"+suffix, func(t *testing.T) {
			fake := newFakeImagesServer(t, http.StatusOK, successBody(t, pngBytes(t), ""))
			backend := newBackend(t, fake.server.URL+suffix, 0)

			if _, err := backend.Generate(context.Background(), Request{Model: "m", Prompt: "p", Aspect: AspectSquare}); err != nil {
				t.Fatalf("generate: %v", err)
			}
			wantPath := strings.TrimSuffix(suffix, "/") + "/images/generations"
			if got := fake.only(t).path; got != wantPath {
				t.Fatalf("expected path %q, got %q", wantPath, got)
			}
		})
	}
}

func TestOpenAICompatMapsFailuresToProviderKinds(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   provider.ErrorKind
	}{
		{"unauthorized", 401, `{"error":{"type":"authorized_error"}}`, provider.ErrorKindAuthFailed},
		{"forbidden", 403, ``, provider.ErrorKindAuthFailed},
		{"insufficient balance", 402, `{"type":"error","error":{"type":"insufficient_balance_error","http_code":"402"}}`, provider.ErrorKindCreditsExhausted},
		{"minimax content safety", 422, `{"type":"error","error":{"type":"unprocessable_entity_error"}}`, provider.ErrorKindContentRejected},
		{"openai content policy", 400, `{"error":{"code":"content_policy_violation","type":"invalid_request_error"}}`, provider.ErrorKindContentRejected},
		{"openai moderation", 400, `{"error":{"code":"moderation_blocked"}}`, provider.ErrorKindContentRejected},
		{"numeric safety code", 500, `{"error":{"code":1026}}`, provider.ErrorKindContentRejected},
		{"rate limited", 429, `{"error":{"type":"rate_limit_error"}}`, provider.ErrorKindRateLimited},
		{"bad request", 400, `{"error":{"type":"bad_request_error"}}`, provider.ErrorKindBadRequest},
		{"not found", 404, `not json at all`, provider.ErrorKindBadRequest},
		{"server error", 500, `{"error":{"type":"server_error"}}`, provider.ErrorKindUpstreamUnavailable},
		{"bad gateway", 502, ``, provider.ErrorKindUpstreamUnavailable},
		{"request timeout", 408, ``, provider.ErrorKindUpstreamUnavailable},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeImagesServer(t, tc.status, tc.body)
			backend := newBackend(t, fake.server.URL+"/v1", 0)

			_, err := backend.Generate(context.Background(), Request{Model: "m", Prompt: "p", Aspect: AspectSquare})
			if err == nil {
				t.Fatal("expected an error")
			}
			if got := provider.KindOf(err); got != tc.want {
				t.Fatalf("expected kind %s, got %s (%v)", tc.want, got, err)
			}
		})
	}
}

func TestOpenAICompatNeverSurfacesProviderMessageOrToken(t *testing.T) {
	const echoed = "SECRET-PROMPT-ECHO the user asked for something private"
	body := fmt.Sprintf(`{"error":{"type":"bad_request_error","code":"invalid","message":%q}}`, echoed+" "+testToken)
	fake := newFakeImagesServer(t, http.StatusBadRequest, body)

	var logs bytes.Buffer
	backend, err := NewOpenAICompat(fake.server.URL+"/v1", testToken, http.DefaultClient, 0, slog.New(slog.NewTextHandler(&logs, nil)))
	if err != nil {
		t.Fatalf("new backend: %v", err)
	}

	_, err = backend.Generate(context.Background(), Request{Model: "m", Prompt: "p", Aspect: AspectSquare})
	if err == nil {
		t.Fatal("expected an error")
	}

	for _, leaked := range []string{"SECRET-PROMPT-ECHO", "private", testToken} {
		if strings.Contains(err.Error(), leaked) {
			t.Fatalf("error leaked %q: %v", leaked, err)
		}
		if strings.Contains(logs.String(), leaked) {
			t.Fatalf("log leaked %q: %s", leaked, logs.String())
		}
	}
	if !strings.Contains(logs.String(), "invalid") {
		t.Fatalf("expected the machine error code in the log, got: %s", logs.String())
	}
}

func TestOpenAICompatDropsProseMasqueradingAsErrorCode(t *testing.T) {
	body := `{"error":{"code":"please describe the private prompt here","type":"x y"}}`
	fake := newFakeImagesServer(t, http.StatusBadRequest, body)

	var logs bytes.Buffer
	backend, err := NewOpenAICompat(fake.server.URL+"/v1", testToken, http.DefaultClient, 0, slog.New(slog.NewTextHandler(&logs, nil)))
	if err != nil {
		t.Fatalf("new backend: %v", err)
	}

	if _, err := backend.Generate(context.Background(), Request{Model: "m", Prompt: "p", Aspect: AspectSquare}); err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(logs.String(), "private prompt") {
		t.Fatalf("free-form provider text reached the log: %s", logs.String())
	}
}

func TestOpenAICompatItemLevelErrors(t *testing.T) {
	cases := []struct {
		name string
		body string
		want provider.ErrorKind
	}{
		{"generated image failed safety", `{"data":[{"error":{"code":1027,"message":"x"}}]}`, provider.ErrorKindContentRejected},
		{"prompt failed safety", `{"data":[{"error":{"code":"1026","message":"x"}}]}`, provider.ErrorKindContentRejected},
		{"generation timed out", `{"data":[{"error":{"code":1001,"message":"x"}}]}`, provider.ErrorKindUpstreamUnavailable},
		{"generation failed", `{"data":[{"error":{"code":1000,"message":"x"}}]}`, provider.ErrorKindUpstreamUnavailable},
		{"unplaceable item error", `{"data":[{"error":{"code":777,"message":"x"}}]}`, provider.ErrorKindUnknown},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeImagesServer(t, http.StatusOK, tc.body)
			backend := newBackend(t, fake.server.URL+"/v1", 0)

			_, err := backend.Generate(context.Background(), Request{Model: "m", Prompt: "p", Aspect: AspectSquare})
			if got := provider.KindOf(err); got != tc.want {
				t.Fatalf("expected kind %s, got %s (%v)", tc.want, got, err)
			}
		})
	}
}

func TestOpenAICompatMalformedSuccessBodies(t *testing.T) {
	cases := map[string]string{
		"url only":         `{"data":[{"url":"https://cdn.example/x.png"}]}`,
		"bad base64":       `{"data":[{"b64_json":"!!!not-base64!!!"}]}`,
		"blank b64":        `{"data":[{"b64_json":"   "}]}`,
		"empty data":       `{"data":[]}`,
		"missing data":     `{}`,
		"not json":         `<html>gateway</html>`,
		"null item error":  `{"data":[{"error":null}]}`,
		"wrong data shape": `{"data":"nope"}`,
	}

	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			fake := newFakeImagesServer(t, http.StatusOK, body)
			backend := newBackend(t, fake.server.URL+"/v1", 0)

			_, err := backend.Generate(context.Background(), Request{Model: "m", Prompt: "p", Aspect: AspectSquare})
			if !errors.Is(err, ErrMalformedResponse) {
				t.Fatalf("expected ErrMalformedResponse, got %v", err)
			}
		})
	}
}

func TestOpenAICompatBoundsResponseBody(t *testing.T) {
	// maxImageBytes 100 allows roughly 1 MiB of body (envelope allowance);
	// a 3 MiB body must be rejected without being buffered whole.
	huge := strings.Repeat("A", 3<<20)
	fake := newFakeImagesServer(t, http.StatusOK, `{"data":[{"b64_json":"`+huge+`"}]}`)
	backend := newBackend(t, fake.server.URL+"/v1", 100)

	_, err := backend.Generate(context.Background(), Request{Model: "m", Prompt: "p", Aspect: AspectSquare})
	if !errors.Is(err, ErrMalformedResponse) {
		t.Fatalf("expected ErrMalformedResponse for an oversized body, got %v", err)
	}
}

func TestOpenAICompatBoundsErrorBodyToo(t *testing.T) {
	fake := newFakeImagesServer(t, http.StatusInternalServerError, strings.Repeat("x", 3<<20))
	backend := newBackend(t, fake.server.URL+"/v1", 100)

	_, err := backend.Generate(context.Background(), Request{Model: "m", Prompt: "p", Aspect: AspectSquare})
	if !errors.Is(err, ErrMalformedResponse) {
		t.Fatalf("expected the size bound to apply before the status verdict, got %v", err)
	}
}

type failingDoer struct{ err error }

func (d failingDoer) Do(*http.Request) (*http.Response, error) { return nil, d.err }

func TestOpenAICompatTransportErrorIsSanitized(t *testing.T) {
	cause := errors.New("dial tcp https://user:" + testToken + "@images.internal:443: connection refused")
	backend, err := NewOpenAICompat("https://images.internal/v1", testToken, failingDoer{err: cause}, 0, nil)
	if err != nil {
		t.Fatalf("new backend: %v", err)
	}

	_, err = backend.Generate(context.Background(), Request{Model: "m", Prompt: "p", Aspect: AspectSquare})
	if err == nil {
		t.Fatal("expected a transport error")
	}
	if got := provider.KindOf(err); got != provider.ErrorKindUpstreamUnavailable {
		t.Fatalf("expected upstream_unavailable, got %s", got)
	}
	if strings.Contains(err.Error(), testToken) || strings.Contains(err.Error(), "images.internal") {
		t.Fatalf("transport error leaked cause details: %v", err)
	}
	if errors.Is(err, cause) {
		t.Fatal("the raw transport cause must not stay in the error chain")
	}
}

func TestOpenAICompatKeepsContextSentinels(t *testing.T) {
	backend, err := NewOpenAICompat("https://images.internal/v1", testToken, failingDoer{err: context.DeadlineExceeded}, 0, nil)
	if err != nil {
		t.Fatalf("new backend: %v", err)
	}

	_, err = backend.Generate(context.Background(), Request{Model: "m", Prompt: "p", Aspect: AspectSquare})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected the deadline sentinel to survive sanitizing, got %v", err)
	}
	if got := provider.KindOf(err); got != provider.ErrorKindUpstreamUnavailable {
		t.Fatalf("expected upstream_unavailable, got %s", got)
	}
}

func TestNewOpenAICompatValidatesConfiguration(t *testing.T) {
	cases := []struct {
		name    string
		baseURL string
		token   string
		client  Doer
	}{
		{"empty url", "", testToken, http.DefaultClient},
		{"no host", "https://", testToken, http.DefaultClient},
		{"unsupported scheme", "ftp://images.example/v1", testToken, http.DefaultClient},
		{"blank token", "https://images.example/v1", "  ", http.DefaultClient},
		{"nil client", "https://images.example/v1", testToken, nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewOpenAICompat(tc.baseURL, tc.token, tc.client, 0, nil)
			if !errors.Is(err, ErrMisconfigured) {
				t.Fatalf("expected ErrMisconfigured, got %v", err)
			}
			if strings.Contains(err.Error(), testToken) {
				t.Fatalf("configuration error leaked the token: %v", err)
			}
		})
	}
}

func TestNewOpenAICompatRedactsURLCredentialsInErrors(t *testing.T) {
	_, err := NewOpenAICompat("ftp://user:hunter2@images.example/v1", testToken, http.DefaultClient, 0, nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("configuration error leaked URL credentials: %v", err)
	}
}

func TestOpenAICompatOmitResponseFormat(t *testing.T) {
	t.Parallel()

	fake := newFakeImagesServer(t, http.StatusOK, successBody(t, pngBytes(t), ""))
	backend := newBackend(t, fake.server.URL+"/v1", 0).OmitResponseFormat()

	if _, err := backend.Generate(context.Background(), Request{Model: "gpt-image-1", Prompt: "a cat", Aspect: AspectSquare}); err != nil {
		t.Fatalf("generate: %v", err)
	}
	if _, present := fake.only(t).body["response_format"]; present {
		t.Fatal("response_format must not be sent when omitted by configuration")
	}
}
