package llm

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"telegram-ollama-reply-bot/internal/config"
)

type stubBackend struct {
	name          string
	caps          Capabilities
	lastRequest   Request
	response      Response
	models        []string
	generateErr   error
	listModelsErr error
}

func (b *stubBackend) Name() string               { return b.name }
func (b *stubBackend) Capabilities() Capabilities { return b.caps }
func (b *stubBackend) Generate(_ context.Context, req Request) (Response, error) {
	b.lastRequest = req

	return b.response, b.generateErr
}
func (b *stubBackend) ListModels(context.Context) ([]string, error) {
	return b.models, b.listModelsErr
}

func TestServiceRoutesFeaturesToConfiguredBackends(t *testing.T) {
	t.Parallel()

	openaiBackend := &stubBackend{
		name: config.LLMBackendOpenAICompat,
		caps: Capabilities{ToolCalls: true, ImageInput: true, ModelListing: true},
		response: Response{
			Backend: config.LLMBackendOpenAICompat,
			Model:   "gpt-4.1-mini",
			Message: TextMessage(RoleAssistant, "summary"),
		},
	}
	ollamaBackend := &stubBackend{
		name: config.LLMBackendOllama,
		caps: Capabilities{ToolCalls: true, ImageInput: true, ModelListing: true},
		response: Response{
			Backend: config.LLMBackendOllama,
			Model:   "gemma3:27b",
			Message: TextMessage(RoleAssistant, "chat"),
		},
	}

	service := &Service{
		cfg: config.LLMConfig{
			Features: config.FeatureConfig{
				Chat:             config.FeatureRouteConfig{Backend: config.LLMBackendOllama, Model: "gemma3:27b"},
				Summarize:        config.FeatureRouteConfig{Backend: config.LLMBackendOpenAICompat, Model: "gpt-4.1-mini"},
				ImageRecognition: config.FeatureRouteConfig{Backend: config.LLMBackendOllama, Model: "gemma3:27b"},
				ToolUse:          config.FeatureRouteConfig{Backend: config.LLMBackendOllama, Model: "gemma3:27b"},
			},
		},
		logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		backends: map[string]backend{config.LLMBackendOpenAICompat: openaiBackend, config.LLMBackendOllama: ollamaBackend},
	}

	resp, err := service.Generate(context.Background(), Request{
		Feature:  FeatureSummarize,
		Messages: []Message{TextMessage(RoleUser, "summarize this")},
	})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}

	if resp.Backend != config.LLMBackendOpenAICompat {
		t.Fatalf("unexpected summarize backend: %s", resp.Backend)
	}

	if openaiBackend.lastRequest.Model != "gpt-4.1-mini" {
		t.Fatalf("unexpected summarize model: %s", openaiBackend.lastRequest.Model)
	}

	_, err = service.Generate(context.Background(), Request{
		Feature: FeatureChat,
		Messages: []Message{
			TextMessage(RoleUser, "hello"),
		},
	})
	if err != nil {
		t.Fatalf("generate chat: %v", err)
	}

	if ollamaBackend.lastRequest.Model != "gemma3:27b" {
		t.Fatalf("unexpected chat model: %s", ollamaBackend.lastRequest.Model)
	}
}

func TestServiceRejectsUnsupportedCapability(t *testing.T) {
	t.Parallel()

	service := &Service{
		cfg: config.LLMConfig{
			Features: config.FeatureConfig{
				Chat:             config.FeatureRouteConfig{Backend: config.LLMBackendOpenAICompat, Model: "gpt"},
				Summarize:        config.FeatureRouteConfig{Backend: config.LLMBackendOpenAICompat, Model: "gpt"},
				ImageRecognition: config.FeatureRouteConfig{Backend: config.LLMBackendOpenAICompat, Model: "gpt"},
				ToolUse:          config.FeatureRouteConfig{Backend: config.LLMBackendOpenAICompat, Model: "gpt"},
			},
		},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		backends: map[string]backend{
			config.LLMBackendOpenAICompat: &stubBackend{
				name: config.LLMBackendOpenAICompat,
				caps: Capabilities{ToolCalls: false, ImageInput: false, ModelListing: true},
			},
		},
	}

	_, err := service.Generate(context.Background(), Request{
		Feature: FeatureToolUse,
		Messages: []Message{
			TextMessage(RoleUser, "hi"),
		},
		Tools: []ToolDefinition{{Name: "x"}},
	})
	if !errors.Is(err, ErrUnsupportedCapability) {
		t.Fatalf("expected unsupported capability error, got %v", err)
	}
}
