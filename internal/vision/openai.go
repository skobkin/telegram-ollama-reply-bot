package vision

import (
	"context"
	"strings"

	"telegram-ollama-reply-bot/internal/config"
	"telegram-ollama-reply-bot/internal/llm"
)

// Generator is the slice of *llm.Service that the OpenAI-compatible recognizer
// needs. The shared conversational service and a separately configured image
// service both satisfy it; keeping the interface local lets this package stay
// decoupled from llm internals (and fakeable in tests).
type Generator interface {
	Generate(ctx context.Context, req llm.Request) (llm.Response, error)
}

// OpenAIRecognizer describes images through an OpenAI-compatible multimodal
// chat completion routed by the llm service's image_recognition feature. It is
// the extraction of the former llm.Service.RecognizeImage request, with the
// image MIME type taken from the sniffed value instead of a hardcoded
// image/jpeg.
type OpenAIRecognizer struct {
	generator Generator
	// resolver resolves the runtime image-recognition model override. Nil
	// keeps the environment route; an override of "" degrades the same way.
	resolver llm.ModelResolver
}

// NewOpenAIRecognizer wraps a Generator — the shared main *llm.Service by
// default, or a separately configured one when the operator set a dedicated
// image endpoint — and the runtime model resolver. Each Recognize call is
// one recognition operation: the model resolves once at its start, and a
// resolution failure fails that operation instead of silently falling back.
func NewOpenAIRecognizer(generator Generator, resolver llm.ModelResolver) *OpenAIRecognizer {
	return &OpenAIRecognizer{generator: generator, resolver: resolver}
}

func (r *OpenAIRecognizer) Name() string { return config.LLMBackendOpenAICompat }

func (r *OpenAIRecognizer) Supports(mime string) bool {
	_, ok := supportedMIME[mime]

	return ok
}

func (r *OpenAIRecognizer) RequiresModel() bool { return true }

func (r *OpenAIRecognizer) Recognize(ctx context.Context, image Image, prompt string) (Recognition, error) {
	if strings.TrimSpace(prompt) == "" {
		return Recognition{}, ErrPromptEmpty
	}

	model := ""
	if r.resolver != nil {
		// The image model is a global-only setting, so the chat scoping
		// value is irrelevant here.
		resolved, err := r.resolver.ResolveModel(ctx, llm.FeatureImageRecognition, 0)
		if err != nil {
			return Recognition{}, err
		}
		model = resolved
	}

	req := llm.Request{
		Feature: llm.FeatureImageRecognition,
		Model:   model,
		Messages: []llm.Message{
			llm.TextMessage(llm.RoleSystem, prompt),
			{
				Role: llm.RoleUser,
				Parts: []llm.Part{
					{
						Type:      llm.PartTypeImage,
						MIMEType:  image.MIME,
						ImageData: image.Data,
					},
				},
			},
		},
	}

	resp, err := r.generator.Generate(ctx, req)
	if err != nil {
		return Recognition{}, err
	}

	if resp.Message.Text() == "" && len(resp.Message.ToolCalls) == 0 {
		return Recognition{}, llm.ErrNoChoices
	}

	usage := resp.Usage

	return Recognition{Text: resp.Message.Text(), Usage: &usage}, nil
}
