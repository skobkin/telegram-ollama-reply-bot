package llm

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
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

func TestServiceHandleChatMessageBuildsRequestFromCompactContext(t *testing.T) {
	t.Parallel()

	backendStub := &stubBackend{
		name: config.LLMBackendOpenAICompat,
		caps: Capabilities{ToolCalls: true, ImageInput: true, ModelListing: true},
		response: Response{
			Backend: config.LLMBackendOpenAICompat,
			Model:   "gpt",
			Message: TextMessage(RoleAssistant, "reply"),
		},
	}

	templateProcessor, err := NewTemplateProcessor(config.PromptConfig{
		ChatSystemPrompt:       "Model={{.Model}}\n{{.Context}}",
		SummarizePrompt:        "{{.Language}}",
		ImageRecognitionPrompt: "{{.Language}}",
		Language:               "English",
		Gender:                 "neutral",
		MaxSummaryLength:       100,
	})
	if err != nil {
		t.Fatalf("NewTemplateProcessor: %v", err)
	}

	service := &Service{
		cfg: config.LLMConfig{
			Features: config.FeatureConfig{
				Chat: config.FeatureRouteConfig{Backend: config.LLMBackendOpenAICompat, Model: "gpt"},
			},
		},
		templateProcessor: templateProcessor,
		logger:            slog.New(slog.NewTextHandler(io.Discard, nil)),
		backends: map[string]backend{
			config.LLMBackendOpenAICompat: backendStub,
		},
	}

	reply, usage, err := service.HandleChatMessage(context.Background(), ChatReplyContext{
		SystemHint:     "compact context",
		EarlierSummary: "earlier summary",
		History: []Message{
			TextMessage(RoleUser, "history 1"),
			TextMessage(RoleAssistant, "history 2"),
		},
		UserMessage: TextMessage(RoleUser, "current"),
	})
	if err != nil {
		t.Fatalf("HandleChatMessage: %v", err)
	}
	if reply != "reply" {
		t.Fatalf("unexpected reply: %q", reply)
	}
	if usage == nil {
		t.Fatal("expected usage")
	}

	messages := backendStub.lastRequest.Messages
	if len(messages) != 5 {
		t.Fatalf("unexpected message count: %d", len(messages))
	}
	if got := messages[0].Text(); !strings.Contains(got, "compact context") {
		t.Fatalf("expected compact system context, got %q", got)
	}
	if got := messages[1].Text(); got != "[Earlier conversation summary: earlier summary]" {
		t.Fatalf("unexpected summary message: %q", got)
	}
	if got := messages[2].Text(); got != "history 1" {
		t.Fatalf("unexpected first history message: %q", got)
	}
	if got := messages[3].Text(); got != "history 2" {
		t.Fatalf("unexpected second history message: %q", got)
	}
	if got := messages[4].Text(); got != "current" {
		t.Fatalf("unexpected current user message: %q", got)
	}
}
