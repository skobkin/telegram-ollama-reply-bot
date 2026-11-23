package tooluse

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"telegram-ollama-reply-bot/internal/config"
	"telegram-ollama-reply-bot/internal/imagegen"

	tg "github.com/mymmrac/telego"
)

type providerCall struct {
	path string
	auth string
	body map[string]any
}

// fakeImageProvider is an OpenAI-compatible images endpoint.
type fakeImageProvider struct {
	server *httptest.Server
	mu     sync.Mutex
	calls  []providerCall
}

func newFakeImageProvider(t *testing.T, status int, body func() string) *fakeImageProvider {
	t.Helper()

	p := &fakeImageProvider{}
	p.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var decoded map[string]any
		_ = json.Unmarshal(raw, &decoded)

		p.mu.Lock()
		p.calls = append(p.calls, providerCall{path: r.URL.Path, auth: r.Header.Get("Authorization"), body: decoded})
		p.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body())
	}))
	t.Cleanup(p.server.Close)

	return p
}

func (p *fakeImageProvider) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()

	return len(p.calls)
}

func realImageService(t *testing.T, baseURL string) *imagegen.Service {
	t.Helper()

	svc, err := imagegen.NewFromConfig(config.ImageGenerationConfig{
		Backend:       config.ImageGenerationBackendOpenAICompat,
		OpenAICompat:  config.ImageGenerationOpenAICompatConfig{BaseURL: baseURL, APIToken: "integration-token"},
		Timeout:       time.Minute,
		MaxImageBytes: 1 << 20,
		MaxConcurrent: 2,
	}, http.DefaultClient, nil)
	if err != nil {
		t.Fatalf("new image service: %v", err)
	}

	return svc
}

// The whole chain with only the network and Telegram faked: the model's tool
// call, the turn snapshot's model, the real service and client, the wire
// request, the decoded image, and the photo that reaches Telegram.
func TestGenerateImageEndToEndThroughTheOpenAICompatibleBackend(t *testing.T) {
	png := testPNG(t)
	provider := newFakeImageProvider(t, http.StatusOK, func() string {
		raw, _ := json.Marshal(map[string]any{"data": []map[string]any{{"b64_json": base64.StdEncoding.EncodeToString(png), "revised_prompt": "a very detailed cat"}}})

		return string(raw)
	})

	stub := &stubLLM{responses: imageToolCallResponses(`{"prompt":"a cat in a hat","aspect":"landscape","caption":"Whiskers"}`)}
	sender := &stubActionSender{message: &tg.Message{MessageID: 501}}
	r := imageLoopRuntime(t, stub, realImageService(t, provider.server.URL+"/v1"), sender, time.Minute)

	result, err := r.ReplyWithTools(t.Context(), imageLoopRequest(true, "MiniMax-H3-Image"))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.ToolCalls) != 1 || result.ToolCalls[0].Status != "ok" || !result.ToolCalls[0].Executed {
		t.Fatalf("records = %+v", result.ToolCalls)
	}

	if provider.callCount() != 1 {
		t.Fatalf("provider calls = %d", provider.callCount())
	}
	call := provider.calls[0]
	if call.path != "/v1/images/generations" || call.auth != "Bearer integration-token" {
		t.Fatalf("unexpected wire request line: %+v", call)
	}
	if call.body["model"] != "MiniMax-H3-Image" || call.body["prompt"] != "a cat in a hat" || call.body["size"] != "1536x1024" || call.body["response_format"] != "b64_json" || call.body["n"] != float64(1) {
		t.Fatalf("unexpected wire body: %v", call.body)
	}

	if sender.photoCalls != 1 || sender.lastPhotoParams.Caption != "Whiskers" || sender.lastPhotoParams.ChatID.ID != 7 || sender.lastPhotoParams.MessageThreadID != 3 {
		t.Fatalf("photo calls = %d params = %+v", sender.photoCalls, sender.lastPhotoParams)
	}
	sent, err := io.ReadAll(sender.lastPhotoParams.Photo.File)
	if err != nil || string(sent) != string(png) {
		t.Fatalf("the photo must be the provider's decoded image (err=%v, %d vs %d bytes)", err, len(sent), len(png))
	}

	// The model sees a compact, byte-free result.
	extras := stub.lastBuildExtras
	if len(extras) == 0 {
		t.Fatal("expected the tool result to be fed back to the model")
	}
	fed := extras[len(extras)-1].Text()
	if !strings.Contains(fed, `"status":"ok"`) || strings.Contains(fed, base64.StdEncoding.EncodeToString(png)) {
		t.Fatalf("unexpected tool result for the model: %s", fed)
	}
}

func TestGenerateImageEndToEndContentRejectionSendsNothing(t *testing.T) {
	const echoed = "SECRET-PROMPT-ECHO private details"
	provider := newFakeImageProvider(t, http.StatusUnprocessableEntity, func() string {
		return `{"type":"error","error":{"type":"unprocessable_entity_error","message":"` + echoed + ` (1026)","http_code":"422"}}`
	})

	stub := &stubLLM{responses: imageToolCallResponses(`{"prompt":"something the provider refuses"}`)}
	sender := &stubActionSender{message: &tg.Message{MessageID: 502}}
	r := imageLoopRuntime(t, stub, realImageService(t, provider.server.URL+"/v1"), sender, time.Minute)

	result, err := r.ReplyWithTools(t.Context(), imageLoopRequest(true, "MiniMax-H3-Image"))
	if err != nil {
		t.Fatal(err)
	}
	if sender.photoCalls != 0 {
		t.Fatal("a refused prompt must never send a photo")
	}
	if len(result.ToolCalls) != 1 || result.ToolCalls[0].ErrorCode != "content_rejected" || result.ToolCalls[0].Retryable {
		t.Fatalf("records = %+v", result.ToolCalls)
	}
	fed := stub.lastBuildExtras[len(stub.lastBuildExtras)-1].Text()
	if strings.Contains(fed, "SECRET-PROMPT-ECHO") || strings.Contains(fed, "private details") {
		t.Fatalf("provider text leaked to the model: %s", fed)
	}
	if provider.callCount() != 1 {
		t.Fatalf("exactly one billed request expected, got %d", provider.callCount())
	}
}

func TestGenerateImageEndToEndProviderOutageIsNotRetried(t *testing.T) {
	provider := newFakeImageProvider(t, http.StatusBadGateway, func() string { return `bad gateway` })

	stub := &stubLLM{responses: imageToolCallResponses(`{"prompt":"a cat"}`)}
	sender := &stubActionSender{message: &tg.Message{MessageID: 503}}
	r := imageLoopRuntime(t, stub, realImageService(t, provider.server.URL+"/v1"), sender, 0)

	result, err := r.ReplyWithTools(t.Context(), imageLoopRequest(true, "MiniMax-H3-Image"))
	if err != nil {
		t.Fatal(err)
	}
	if provider.callCount() != 1 || sender.photoCalls != 0 {
		t.Fatalf("an outage must neither be retried nor send anything: calls=%d photos=%d", provider.callCount(), sender.photoCalls)
	}
	if len(result.ToolCalls) != 1 || result.ToolCalls[0].ErrorCode != "provider_unavailable" || result.ToolCalls[0].ErrorClass != ErrorTransient {
		t.Fatalf("records = %+v", result.ToolCalls)
	}
}
