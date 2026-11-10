package vision

import (
	"context"
	"errors"
	"testing"

	"telegram-ollama-reply-bot/internal/config"
	"telegram-ollama-reply-bot/internal/llm"
)

// fakeGenerator captures the request passed through the Generator interface.
type fakeGenerator struct {
	request     llm.Request
	response    llm.Response
	generateErr error
}

func (g *fakeGenerator) Generate(_ context.Context, req llm.Request) (llm.Response, error) {
	g.request = req

	return g.response, g.generateErr
}

// pngImage returns a minimal PNG-signature payload — enough for the
// recognizer, which only forwards the sniffed MIME and bytes.
func pngImage() Image {
	return Image{
		Data: append([]byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}, []byte("body")...),
		MIME: "image/png",
	}
}

// TestOpenAIRecognizerSendsSniffedMIME is the regression for the former
// hardcoded image/jpeg: the image part must carry the MIME detected from the
// actual bytes, so the adapter's data URL is built from the real format.
func TestOpenAIRecognizerSendsSniffedMIME(t *testing.T) {
	t.Parallel()

	gen := &fakeGenerator{
		response: llm.Response{
			Backend: config.LLMBackendOpenAICompat,
			Model:   "vision-model",
			Message: llm.TextMessage(llm.RoleAssistant, "a test chart"),
		},
	}
	recognizer := NewOpenAIRecognizer(gen)

	recognition, err := recognizer.Recognize(context.Background(), pngImage(), "describe the image")
	if err != nil {
		t.Fatalf("recognize: %v", err)
	}

	req := gen.request
	if req.Feature != llm.FeatureImageRecognition {
		t.Fatalf("expected image_recognition feature, got %s", req.Feature)
	}
	if len(req.Messages) != 2 {
		t.Fatalf("expected system+user messages, got %d", len(req.Messages))
	}
	if got := req.Messages[0].Text(); got != "describe the image" {
		t.Fatalf("expected prompt as system message, got %q", got)
	}
	if req.Messages[1].Role != llm.RoleUser {
		t.Fatalf("expected user role, got %s", req.Messages[1].Role)
	}
	if len(req.Messages[1].Parts) != 1 {
		t.Fatalf("expected single image part, got %d", len(req.Messages[1].Parts))
	}
	part := req.Messages[1].Parts[0]
	if part.Type != llm.PartTypeImage {
		t.Fatalf("expected image part, got %s", part.Type)
	}
	if part.MIMEType != "image/png" {
		t.Fatalf("expected sniffed image/png mime (regression vs hardcoded jpeg), got %s", part.MIMEType)
	}
	if string(part.ImageData) != string(pngImage().Data) {
		t.Fatalf("expected image bytes forwarded unchanged")
	}

	if recognition.Text != "a test chart" {
		t.Fatalf("unexpected description: %q", recognition.Text)
	}
	if recognition.Usage == nil {
		t.Fatalf("expected usage propagated from the backend response")
	}
}

func TestOpenAIRecognizerRejectsEmptyPrompt(t *testing.T) {
	t.Parallel()

	gen := &fakeGenerator{}
	recognizer := NewOpenAIRecognizer(gen)

	_, err := recognizer.Recognize(context.Background(), pngImage(), "  ")

	if !errors.Is(err, ErrPromptEmpty) {
		t.Fatalf("expected ErrPromptEmpty, got %v", err)
	}
	if gen.request.Feature != "" {
		t.Fatalf("expected no request issued, got feature %s", gen.request.Feature)
	}
}

func TestOpenAIRecognizerNoChoices(t *testing.T) {
	t.Parallel()

	recognizer := NewOpenAIRecognizer(&fakeGenerator{})

	_, err := recognizer.Recognize(context.Background(), pngImage(), "describe")

	if !errors.Is(err, llm.ErrNoChoices) {
		t.Fatalf("expected ErrNoChoices, got %v", err)
	}
}

func TestOpenAIRecognizerRequiresModel(t *testing.T) {
	t.Parallel()

	recognizer := NewOpenAIRecognizer(&fakeGenerator{})

	if recognizer.Name() != config.LLMBackendOpenAICompat {
		t.Fatalf("unexpected name: %s", recognizer.Name())
	}
	if !recognizer.RequiresModel() {
		t.Fatalf("openai recognizer must require a feature model")
	}
	if !recognizer.Supports("image/webp") || recognizer.Supports("image/gif") {
		t.Fatalf("unexpected Supports results")
	}
}
