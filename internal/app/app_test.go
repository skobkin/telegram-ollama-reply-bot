package app

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"telegram-ollama-reply-bot/internal/config"
	"telegram-ollama-reply-bot/internal/llm"
	"telegram-ollama-reply-bot/internal/state"
	"telegram-ollama-reply-bot/internal/tooluse"
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
	}, renderer, slog.New(slog.NewTextHandler(io.Discard, nil)))
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
