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
}

// NewOpenAIRecognizer wraps a Generator: the shared main *llm.Service by
// default, or a separately configured one when the operator set a dedicated
// image endpoint.
func NewOpenAIRecognizer(generator Generator) *OpenAIRecognizer {
	return &OpenAIRecognizer{generator: generator}
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

	resp, err := r.generator.Generate(ctx, llm.Request{
		Feature: llm.FeatureImageRecognition,
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
	})
	if err != nil {
		return Recognition{}, err
	}

	if resp.Message.Text() == "" && len(resp.Message.ToolCalls) == 0 {
		return Recognition{}, llm.ErrNoChoices
	}

	usage := resp.Usage

	return Recognition{Text: resp.Message.Text(), Usage: &usage}, nil
}
