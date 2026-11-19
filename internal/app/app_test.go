package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tg "github.com/mymmrac/telego"

	"telegram-ollama-reply-bot/internal/config"
	"telegram-ollama-reply-bot/internal/content/search"
	"telegram-ollama-reply-bot/internal/llm"
	"telegram-ollama-reply-bot/internal/provider"
	"telegram-ollama-reply-bot/internal/state"
	"telegram-ollama-reply-bot/internal/support/httpclient"
	"telegram-ollama-reply-bot/internal/tooluse"
	"telegram-ollama-reply-bot/internal/vision"
)

// modelServer is a fake OpenAI-compatible backend serving /models. It counts
// requests so tests can prove which connection the startup gate actually
// consulted.
type modelServer struct {
	server   *httptest.Server
	requests atomic.Int32
}

func newModelServer(t *testing.T, models ...string) *modelServer {
	t.Helper()

	fake := &modelServer{}
	fake.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fake.requests.Add(1)

		data := make([]map[string]any, 0, len(models))
		for _, m := range models {
			data = append(data, map[string]any{"id": m, "object": "model", "owned_by": "test"})
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
	}))
	t.Cleanup(fake.server.Close)

	return fake
}

// newModelService builds a service that always requests the models
// "chat-model", "summary-model", and "image-model"; each test controls which
// of them the fake server actually serves.
func newModelService(t *testing.T, server *modelServer) *llm.Service {
	t.Helper()

	renderer, err := llm.NewStaticPromptRenderer(true)
	if err != nil {
		t.Fatalf("static renderer: %v", err)
	}

	service, err := llm.NewService(config.LLMConfig{
		Backends: config.BackendConfig{
			OpenAICompat: config.OpenAICompatBackendConfig{BaseURL: server.server.URL + "/v1"},
		},
		Features: config.FeatureConfig{
			Chat:             config.FeatureRouteConfig{Model: "chat-model"},
			Summarize:        config.FeatureRouteConfig{Model: "summary-model"},
			ImageRecognition: config.FeatureRouteConfig{Model: "image-model"},
		},
	}, renderer, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("new service: %v", err)
	}

	return service
}

// TestVerifyStartupModels_ImageCheckedAgainstActualConnection is the
// two-server regression: with a separately configured image connection, an
// image-only model absent from the chat server must not block the boot (it is
// checked against the image server), while the same model missing from the
// actual image server must.
func TestVerifyStartupModels_ImageCheckedAgainstActualConnection(t *testing.T) {
	t.Parallel()

	// The chat server has no image model; the image server has it.
	chatServer := newModelServer(t, "chat-model", "summary-model")
	imageServer := newModelServer(t, "image-model")

	main := newModelService(t, chatServer)
	image := newModelService(t, imageServer)

	hasAll, result := verifyStartupModels(context.Background(), main, image, true)
	if !hasAll {
		t.Fatalf("expected boot to pass: image model lives on the image connection, got %v", result)
	}
	if imageServer.requests.Load() == 0 {
		t.Fatalf("expected the image model to be checked against the image server")
	}
}

func TestVerifyStartupModels_MissingImageModelOnImageServerFails(t *testing.T) {
	t.Parallel()

	chatServer := newModelServer(t, "chat-model", "summary-model")
	imageServer := newModelServer(t, "other-model")

	main := newModelService(t, chatServer)
	image := newModelService(t, imageServer)

	hasAll, result := verifyStartupModels(context.Background(), main, image, true)
	if hasAll {
		t.Fatalf("expected boot to fail when the image server lacks the image model")
	}
	if result[string(llm.FeatureImageRecognition)] {
		t.Fatalf("expected image_recognition marked unavailable, got %v", result)
	}
}

// TestVerifyStartupModels_SharedConnectionChecksAllThree preserves today's
// semantics for the default route: with no separate image service, the main
// connection is checked for all three features.
func TestVerifyStartupModels_SharedConnectionChecksAllThree(t *testing.T) {
	t.Parallel()

	chatServer := newModelServer(t, "chat-model", "summary-model")

	main := newModelService(t, chatServer)

	hasAll, result := verifyStartupModels(context.Background(), main, nil, true)
	if hasAll {
		t.Fatalf("expected boot to fail when the shared server lacks the image model")
	}
	if result[string(llm.FeatureImageRecognition)] {
		t.Fatalf("expected image_recognition marked unavailable, got %v", result)
	}
}

// TestVerifyStartupModels_ModelFreeRouteSkipsImageCheck covers the MiniMax
// recognizer and disabled recognition: no image model is required anywhere,
// and the image connection (if one was even built) is never consulted.
func TestVerifyStartupModels_ModelFreeRouteSkipsImageCheck(t *testing.T) {
	t.Parallel()

	chatServer := newModelServer(t, "chat-model", "summary-model")
	// The image server lists nothing relevant; it must not be contacted.
	imageServer := newModelServer(t)

	main := newModelService(t, chatServer)
	image := newModelService(t, imageServer)

	hasAll, result := verifyStartupModels(context.Background(), main, image, false)
	if !hasAll {
		t.Fatalf("model-free route must not require an image model, got %v", result)
	}
	if imageServer.requests.Load() != 0 {
		t.Fatalf("image connection must receive zero model checks, got %d", imageServer.requests.Load())
	}
}

func TestImageModelLogValue(t *testing.T) {
	t.Parallel()

	openaiRoute := &config.Config{}
	openaiRoute.LLM.ImageRecognitionEnabled = true
	openaiRoute.LLM.ImageRecognition.Recognizer = config.ImageRecognizerOpenAICompat
	openaiRoute.LLM.Features.ImageRecognition.Model = "vision-model"
	if got := imageModelLogValue(openaiRoute); got != "vision-model" {
		t.Fatalf("expected vision-model, got %q", got)
	}

	minimaxRoute := &config.Config{}
	minimaxRoute.LLM.ImageRecognitionEnabled = true
	minimaxRoute.LLM.ImageRecognition.Recognizer = config.ImageRecognizerMiniMaxVLM
	minimaxRoute.LLM.Features.ImageRecognition.Model = "vision-model"
	if got := imageModelLogValue(minimaxRoute); got != "" {
		t.Fatalf("model-free route must not log an image model, got %q", got)
	}

	disabled := &config.Config{}
	disabled.LLM.ImageRecognition.Recognizer = config.ImageRecognizerOpenAICompat
	disabled.LLM.Features.ImageRecognition.Model = "vision-model"
	if got := imageModelLogValue(disabled); got != "" {
		t.Fatalf("disabled recognition must not log an image model, got %q", got)
	}
}

type stubAnalyzer struct{}

func (stubAnalyzer) AnalyzeImage(context.Context, state.ImageMeta, string) (string, error) {
	return "analysis", nil
}

// TestAttachImageToolsGatedOnEnabledFlag proves the model-prompted image
// tools are registered exactly while image recognition is enabled.
func TestAttachImageToolsGatedOnEnabledFlag(t *testing.T) {
	t.Parallel()

	newRuntime := func() *tooluse.Runtime {
		return tooluse.New(nil, nil, nil, nil, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), tooluse.Config{})
	}

	disabled := newRuntime()
	attachImageTools(disabled, stubAnalyzer{}, false)
	if disabled.HasTool("analyze_image") || disabled.HasTool("list_recent_images") {
		t.Fatal("image tools must stay unregistered while recognition is disabled")
	}

	enabled := newRuntime()
	attachImageTools(enabled, stubAnalyzer{}, true)
	if !enabled.HasTool("analyze_image") || !enabled.HasTool("list_recent_images") {
		t.Fatal("image tools must be registered while recognition is enabled")
	}
}

// stubRecognizer is a vision.Recognizer without ValidateAuth, proving the
// startup credential probe skips recognizers that cannot self-validate.
type stubRecognizer struct{}

func (stubRecognizer) Name() string         { return "stub" }
func (stubRecognizer) Supports(string) bool { return true }
func (stubRecognizer) RequiresModel() bool  { return false }
func (stubRecognizer) Recognize(context.Context, vision.Image, string) (vision.Recognition, error) {
	return vision.Recognition{}, nil
}

// stubValidatingSearcher is a search.Searcher with an independent
// ValidateAuth outcome, used instead of HTTP-level fakes here because the
// HTTP classification behavior is covered in the search package itself.
type stubValidatingSearcher struct {
	validateErr error
	probes      atomic.Int32
}

func (s *stubValidatingSearcher) Search(context.Context, search.Request) (search.Result, error) {
	return search.Result{}, nil
}

func (s *stubValidatingSearcher) ValidateAuth(context.Context) error {
	s.probes.Add(1)

	return s.validateErr
}

func appTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestVerifyRecognizerAuthDefinitiveFails(t *testing.T) {
	t.Parallel()

	// A real MiniMax recognizer against a fake endpoint that reports the
	// provider auth status 1004.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"base_resp":{"status_code":1004,"status_msg":"invalid api key"}}`)
	}))
	t.Cleanup(server.Close)

	recognizer, err := vision.NewMiniMaxRecognizer(server.URL, "test-key", 5*time.Second, appTestLogger())
	if err != nil {
		t.Fatalf("new minimax recognizer: %v", err)
	}

	if err := verifyRecognizerAuth(context.Background(), recognizer, appTestLogger()); !errors.Is(err, vision.ErrRecognizerAuth) {
		t.Fatalf("expected definitive auth failure, got %v", err)
	}
}

func TestVerifyRecognizerAuthTransientContinues(t *testing.T) {
	t.Parallel()

	t.Run("http 500", func(t *testing.T) {
		t.Parallel()

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		t.Cleanup(server.Close)

		recognizer, err := vision.NewMiniMaxRecognizer(server.URL, "test-key", 5*time.Second, appTestLogger())
		if err != nil {
			t.Fatalf("new minimax recognizer: %v", err)
		}

		if err := verifyRecognizerAuth(context.Background(), recognizer, appTestLogger()); err != nil {
			t.Fatalf("transient failure must not abort the boot, got %v", err)
		}
	})

	t.Run("connection refused", func(t *testing.T) {
		t.Parallel()

		recognizer, err := vision.NewMiniMaxRecognizer("http://127.0.0.1:1", "test-key", 5*time.Second, appTestLogger())
		if err != nil {
			t.Fatalf("new minimax recognizer: %v", err)
		}

		if err := verifyRecognizerAuth(context.Background(), recognizer, appTestLogger()); err != nil {
			t.Fatalf("transient failure must not abort the boot, got %v", err)
		}
	})

	t.Run("recognizer without validator", func(t *testing.T) {
		t.Parallel()

		if err := verifyRecognizerAuth(context.Background(), stubRecognizer{}, appTestLogger()); err != nil {
			t.Fatalf("recognizer without ValidateAuth must be skipped, got %v", err)
		}
	})
}

func TestVerifySearcherAuthClassification(t *testing.T) {
	t.Parallel()

	definitive := map[bool]provider.ErrorKind{true: provider.ErrorKindAuthFailed, false: provider.ErrorKindCreditsExhausted}

	for _, kind := range definitive {
		searcher := &stubValidatingSearcher{validateErr: provider.NewError("tavily", kind)}
		err := verifySearcherAuth(context.Background(), searcher, appTestLogger())
		if err == nil {
			t.Fatalf("kind %s must abort the boot", kind)
		}
		if !provider.IsDefinitiveRejection(err) || !strings.Contains(err.Error(), "tavily") {
			t.Fatalf("error must name the provider and kind, got %v", err)
		}
	}

	transient := []provider.ErrorKind{provider.ErrorKindRateLimited, provider.ErrorKindUpstreamUnavailable, provider.ErrorKindUnknown}
	for _, kind := range transient {
		searcher := &stubValidatingSearcher{validateErr: provider.NewError("tavily", kind)}
		if err := verifySearcherAuth(context.Background(), searcher, appTestLogger()); err != nil {
			t.Fatalf("transient kind %s must not abort the boot, got %v", kind, err)
		}
	}

	plain := &stubValidatingSearcher{}
	if err := verifySearcherAuth(context.Background(), struct{ search.Searcher }{plain}, appTestLogger()); err != nil {
		t.Fatalf("searcher without ValidateAuth must be skipped, got %v", err)
	}
	if plain.probes.Load() != 0 {
		t.Fatalf("searcher without ValidateAuth must not be probed, got %d probes", plain.probes.Load())
	}
}

func TestVerifySearcherAuthNilSearcherNoProbe(t *testing.T) {
	t.Parallel()

	if err := verifySearcherAuth(context.Background(), nil, appTestLogger()); err != nil {
		t.Fatalf("disabled search must skip validation, got %v", err)
	}
}

// TestVerifyTelegramIdentitySanitizesFailures pins the credential-leak fix:
// telego's transport errors embed the token-bearing API URL, so a failed
// bootstrap GetMe must surface a sanitized error — in the log line and in
// the returned startup error alike.
func TestVerifyTelegramIdentitySanitizesFailures(t *testing.T) {
	t.Parallel()

	token := "1:" + strings.Repeat("A", 35)
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := dead.URL
	dead.Close() // every request is now refused at transport level

	api, err := tg.NewBot(token, tg.WithAPIServer(url), tg.WithDiscardLogger(),
		tg.WithHTTPClient(httpclient.New()))
	if err != nil {
		t.Fatalf("NewBot: %v", err)
	}

	_, rawErr := api.GetMe(context.Background())
	if rawErr == nil {
		t.Fatal("premise: expected GetMe against a dead server to fail")
	}
	if !strings.Contains(rawErr.Error(), token) {
		t.Fatalf("premise: expected the raw transport error to embed the token-bearing URL, got %q", rawErr.Error())
	}

	logs := &bytes.Buffer{}
	_, err = verifyTelegramIdentity(context.Background(), api, slog.New(slog.NewTextHandler(logs, nil)))
	if err == nil {
		t.Fatal("expected verifyTelegramIdentity to fail")
	}
	if strings.Contains(err.Error(), token) || strings.Contains(err.Error(), url) {
		t.Fatalf("returned startup error must not retain the token-bearing URL: %q", err.Error())
	}
	if logged := logs.String(); strings.Contains(logged, token) || strings.Contains(logged, url) {
		t.Fatalf("log output must not retain the token-bearing URL: %q", logged)
	}
	if !errors.Is(err, ErrTelegramConnect) {
		t.Fatalf("failure must identify the telegram connection: %v", err)
	}
}
