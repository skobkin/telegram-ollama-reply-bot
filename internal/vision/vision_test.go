package vision

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"text/template"

	"telegram-ollama-reply-bot/internal/llm"
)

// fakeRenderer implements llm.PromptRenderer with fixed prompts.
type fakeRenderer struct {
	imagePrompt    string
	renderErr      error
	imagePromptArg llm.PromptScope
}

func (r *fakeRenderer) RenderChatSystemPrompt(_ context.Context, _ llm.PromptScope, _, _, _ string) (string, error) {
	return "chat", nil
}

func (r *fakeRenderer) RenderSummarizeSystemPrompt(_ context.Context, _ llm.PromptScope) (string, error) {
	return "summarize", nil
}

func (r *fakeRenderer) RenderImageRecognitionSystemPrompt(_ context.Context, scope llm.PromptScope) (string, error) {
	r.imagePromptArg = scope

	return r.imagePrompt, r.renderErr
}

// fakeRecognizer captures the dispatched recognition.
type fakeRecognizer struct {
	name          string
	supports      func(mime string) bool
	requiresModel bool

	capturedImage  Image
	capturedPrompt string
	recognition    Recognition
	recognizeErr   error
}

func (r *fakeRecognizer) Name() string { return r.name }

func (r *fakeRecognizer) Supports(mime string) bool {
	if r.supports != nil {
		return r.supports(mime)
	}

	return true
}

func (r *fakeRecognizer) RequiresModel() bool { return r.requiresModel }

func (r *fakeRecognizer) Recognize(_ context.Context, image Image, prompt string) (Recognition, error) {
	r.capturedImage = image
	r.capturedPrompt = prompt

	return r.recognition, r.recognizeErr
}

func testService(t *testing.T, recognizer Recognizer, renderer llm.PromptRenderer, maxImageBytes int64) *Service {
	t.Helper()

	return NewService(recognizer, renderer, maxImageBytes, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestServiceDescribe_RendersPromptAndDispatches(t *testing.T) {
	t.Parallel()

	renderer := &fakeRenderer{imagePrompt: "operator template"}
	recognizer := &fakeRecognizer{name: "fake", recognition: Recognition{Text: "description"}}
	service := testService(t, recognizer, renderer, 1<<20)

	data := append([]byte{0xFF, 0xD8, 0xFF}, []byte("jpeg body")...)

	recognition, err := service.Describe(context.Background(), data)
	if err != nil {
		t.Fatalf("describe: %v", err)
	}

	if recognition.Text != "description" {
		t.Fatalf("unexpected description: %q", recognition.Text)
	}
	if recognizer.capturedPrompt != "operator template" {
		t.Fatalf("expected rendered prompt dispatched, got %q", recognizer.capturedPrompt)
	}
	if recognizer.capturedImage.MIME != "image/jpeg" {
		t.Fatalf("expected sniffed mime, got %s", recognizer.capturedImage.MIME)
	}
	if string(recognizer.capturedImage.Data) != string(data) {
		t.Fatalf("expected bytes forwarded unchanged")
	}
	if renderer.imagePromptArg != (llm.PromptScope{}) {
		t.Fatalf("expected empty prompt scope preserved from the legacy flow, got %+v", renderer.imagePromptArg)
	}
}

func TestServiceDescribe_RejectsOversizedImageBeforeRecognizer(t *testing.T) {
	t.Parallel()

	recognizer := &fakeRecognizer{name: "fake"}
	service := testService(t, recognizer, &fakeRenderer{}, 8)

	_, err := service.Describe(context.Background(), make([]byte, 9))
	if !errors.Is(err, ErrImageTooLarge) {
		t.Fatalf("expected ErrImageTooLarge, got %v", err)
	}
	if recognizer.capturedPrompt != "" {
		t.Fatalf("expected no dispatch, got prompt %q", recognizer.capturedPrompt)
	}
}

func TestServiceDescribe_RejectsUnsupportedTypeBeforeRecognizer(t *testing.T) {
	t.Parallel()

	recognizer := &fakeRecognizer{name: "fake"}
	service := testService(t, recognizer, &fakeRenderer{}, 1<<20)

	_, err := service.Describe(context.Background(), []byte("GIF89a...."))
	if !errors.Is(err, ErrUnsupportedImageType) {
		t.Fatalf("expected ErrUnsupportedImageType, got %v", err)
	}
	if recognizer.capturedPrompt != "" {
		t.Fatalf("expected no dispatch for unsupported type")
	}
}

func TestServiceDescribe_RejectsTypeUnsupportedByRecognizer(t *testing.T) {
	t.Parallel()

	recognizer := &fakeRecognizer{name: "fake", supports: func(mime string) bool { return mime == "image/png" }}
	service := testService(t, recognizer, &fakeRenderer{}, 1<<20)

	_, err := service.Describe(context.Background(), append([]byte{0xFF, 0xD8, 0xFF}, []byte("body")...))
	if !errors.Is(err, ErrUnsupportedImageType) {
		t.Fatalf("expected ErrUnsupportedImageType for recognizer mismatch, got %v", err)
	}
}

func TestServiceDescribe_TemplateErrorKeepsLegacySentinel(t *testing.T) {
	t.Parallel()

	renderer := &fakeRenderer{renderErr: errors.New("template boom")}
	service := testService(t, &fakeRecognizer{name: "fake"}, renderer, 1<<20)

	_, err := service.Describe(context.Background(), append([]byte{0xFF, 0xD8, 0xFF}, []byte("body")...))
	if !errors.Is(err, llm.ErrTemplateProcessing) {
		t.Fatalf("expected ErrTemplateProcessing, got %v", err)
	}
}

// TestServiceDescribe_TemplateErrorDoesNotLeakTemplateText uses a real
// text/template execution failure: the error quotes the offending expression,
// including operator-managed literals, so neither the returned error nor the
// normal-level log may carry it — only the stable template-error kind.
func TestServiceDescribe_TemplateErrorDoesNotLeakTemplateText(t *testing.T) {
	t.Parallel()

	const sentinel = "PRIVATE_ADMIN_TEMPLATE_SENTINEL"
	tmpl := template.Must(template.New("image_recognition").Parse(`{{ index "` + sentinel + `" 999 }}`))
	execErr := tmpl.Execute(io.Discard, nil)
	if execErr == nil {
		t.Fatal("expected template execution error")
	}
	if !strings.Contains(execErr.Error(), sentinel) {
		t.Fatalf("precondition: execution error must echo the template literal, got %v", execErr)
	}

	var logs bytes.Buffer
	service := NewService(
		&fakeRecognizer{name: "fake"},
		&fakeRenderer{renderErr: execErr},
		1<<20,
		slog.New(slog.NewTextHandler(&logs, nil)),
	)

	_, err := service.Describe(context.Background(), append([]byte{0xFF, 0xD8, 0xFF}, []byte("body")...))
	if !errors.Is(err, llm.ErrTemplateProcessing) {
		t.Fatalf("expected ErrTemplateProcessing, got %v", err)
	}
	if strings.Contains(err.Error(), sentinel) {
		t.Fatalf("returned error leaks template text: %q", err.Error())
	}
	if logText := logs.String(); strings.Contains(logText, sentinel) {
		t.Fatalf("template failure log leaks operator-managed template text: %q", logText)
	}
}

func TestServiceDescribe_RecognizerErrorPassesThrough(t *testing.T) {
	t.Parallel()

	recognizer := &fakeRecognizer{name: "fake", recognizeErr: ErrRecognizerUnavailable}
	service := testService(t, recognizer, &fakeRenderer{}, 1<<20)

	_, err := service.Describe(context.Background(), append([]byte{0xFF, 0xD8, 0xFF}, []byte("body")...))
	if !errors.Is(err, ErrRecognizerUnavailable) {
		t.Fatalf("expected recognizer error passthrough, got %v", err)
	}
}

// TestServiceAnalyze_PassesPromptVerbatimWithoutTemplate is the model-authored
// re-recognition path: the prompt reaches the recognizer verbatim, the
// operator template is never rendered (a renderer configured to fail proves
// it is not consulted), and usage propagates.
func TestServiceAnalyze_PassesPromptVerbatimWithoutTemplate(t *testing.T) {
	t.Parallel()

	// The renderer fails if Describe's template path is ever taken.
	renderer := &fakeRenderer{renderErr: errors.New("template must not be rendered")}
	recognizer := &fakeRecognizer{
		name:          "fake",
		recognition:   Recognition{Text: "targeted analysis", Usage: &llm.TokenUsage{TotalTokens: 9}},
		requiresModel: false,
	}
	service := testService(t, recognizer, renderer, 1<<20)

	data := append([]byte{0xFF, 0xD8, 0xFF}, []byte("body")...)

	recognition, err := service.Analyze(context.Background(), data, "read the error text in the image")
	if err != nil {
		t.Fatalf("analyze: %v", err)
	}

	if recognizer.capturedPrompt != "read the error text in the image" {
		t.Fatalf("expected model prompt passed verbatim, got %q", recognizer.capturedPrompt)
	}
	if recognizer.capturedImage.MIME != "image/jpeg" || string(recognizer.capturedImage.Data) != string(data) {
		t.Fatalf("expected validated image forwarded unchanged")
	}
	if recognition.Text != "targeted analysis" || recognition.Usage == nil || recognition.Usage.TotalTokens != 9 {
		t.Fatalf("unexpected recognition: %+v", recognition)
	}
}

// TestServiceAnalyze_RejectsBlankPromptBeforeAnyWork ensures a blank prompt
// fails fast without validation, rendering, or recognition.
func TestServiceAnalyze_RejectsBlankPromptBeforeAnyWork(t *testing.T) {
	t.Parallel()

	recognizer := &fakeRecognizer{name: "fake"}
	service := testService(t, recognizer, &fakeRenderer{}, 1<<20)

	_, err := service.Analyze(context.Background(), append([]byte{0xFF, 0xD8, 0xFF}, []byte("body")...), "  ")
	if !errors.Is(err, ErrPromptEmpty) {
		t.Fatalf("expected ErrPromptEmpty, got %v", err)
	}
	if recognizer.capturedPrompt != "" {
		t.Fatalf("expected no recognizer call for blank prompt")
	}
}

// TestServiceAnalyze_SharesBoundAndMIMEChecks proves Analyze inherits
// Describe's validation core: oversized data and unsupported signatures are
// rejected before any recognizer call.
func TestServiceAnalyze_SharesBoundAndMIMEChecks(t *testing.T) {
	t.Parallel()

	recognizer := &fakeRecognizer{name: "fake"}
	service := testService(t, recognizer, &fakeRenderer{}, 8)

	if _, err := service.Analyze(context.Background(), make([]byte, 9), "look"); !errors.Is(err, ErrImageTooLarge) {
		t.Fatalf("expected ErrImageTooLarge, got %v", err)
	}

	if _, err := service.Analyze(context.Background(), []byte("GIF89a"), "look"); !errors.Is(err, ErrUnsupportedImageType) {
		t.Fatalf("expected ErrUnsupportedImageType, got %v", err)
	}

	if recognizer.capturedPrompt != "" {
		t.Fatalf("expected no recognizer calls for invalid input")
	}
}

func TestService_MetadataDelegates(t *testing.T) {
	t.Parallel()

	modelFree := testService(t, &fakeRecognizer{name: "minimax_vlm", requiresModel: false}, &fakeRenderer{}, 1<<20)
	modelRouted := testService(t, &fakeRecognizer{name: "openai_compat", requiresModel: true}, &fakeRenderer{}, 1<<20)

	if modelFree.Name() != "minimax_vlm" || modelFree.RequiresModel() {
		t.Fatalf("unexpected model-free metadata")
	}
	if modelRouted.Name() != "openai_compat" || !modelRouted.RequiresModel() {
		t.Fatalf("unexpected model-routed metadata")
	}
}

// TestServiceDescribe_ZeroBoundDisablesSizeCheck documents the struct-literal
// escape hatch: production wiring always passes the configured bound.
func TestServiceDescribe_ZeroBoundDisablesSizeCheck(t *testing.T) {
	t.Parallel()

	recognizer := &fakeRecognizer{name: "fake", recognition: Recognition{Text: "ok"}}
	service := testService(t, recognizer, &fakeRenderer{imagePrompt: "p"}, 0)

	// A JPEG signature followed by a large body: over any realistic bound,
	// but the disabled bound must let it through.
	data := append([]byte{0xFF, 0xD8, 0xFF}, make([]byte, 1<<16)...)

	recognition, err := service.Describe(context.Background(), data)
	if err != nil {
		t.Fatalf("describe with disabled bound: %v", err)
	}
	if recognition.Text != "ok" {
		t.Fatalf("unexpected description: %q", recognition.Text)
	}
}
