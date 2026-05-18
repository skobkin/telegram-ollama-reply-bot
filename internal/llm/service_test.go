package llm

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"telegram-ollama-reply-bot/internal/adminconfig"
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

func TestServiceAppliesConfiguredFeatureModels(t *testing.T) {
	t.Parallel()

	backendStub := &stubBackend{
		name: config.LLMBackendOpenAICompat,
		caps: Capabilities{ToolCalls: true, ImageInput: true, ModelListing: true},
		response: Response{
			Backend: config.LLMBackendOpenAICompat,
			Model:   "gpt-4.1-mini",
			Message: TextMessage(RoleAssistant, "summary"),
		},
	}

	service := &Service{
		cfg: config.LLMConfig{
			Features: config.FeatureConfig{
				Chat:             config.FeatureRouteConfig{Model: "gemma3:27b"},
				Summarize:        config.FeatureRouteConfig{Model: "gpt-4.1-mini"},
				ImageRecognition: config.FeatureRouteConfig{Model: "gemma3:27b"},
			},
		},
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		backend: backendStub,
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

	if backendStub.lastRequest.Model != "gpt-4.1-mini" {
		t.Fatalf("unexpected summarize model: %s", backendStub.lastRequest.Model)
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

	if backendStub.lastRequest.Model != "gemma3:27b" {
		t.Fatalf("unexpected chat model: %s", backendStub.lastRequest.Model)
	}
}

func TestServiceRejectsUnsupportedCapability(t *testing.T) {
	t.Parallel()

	service := &Service{
		cfg: config.LLMConfig{
			Features: config.FeatureConfig{
				Chat:             config.FeatureRouteConfig{Model: "gpt"},
				Summarize:        config.FeatureRouteConfig{Model: "gpt"},
				ImageRecognition: config.FeatureRouteConfig{Model: "gpt"},
			},
		},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		backend: &stubBackend{
			name: config.LLMBackendOpenAICompat,
			caps: Capabilities{ToolCalls: false, ImageInput: false, ModelListing: true},
		},
	}

	_, err := service.Generate(context.Background(), Request{
		Feature: FeatureChat,
		Messages: []Message{
			TextMessage(RoleUser, "hi"),
		},
		Tools: []ToolDefinition{{Name: "x"}},
	})
	if !errors.Is(err, ErrUnsupportedCapability) {
		t.Fatalf("expected unsupported capability error, got %v", err)
	}
}

func TestServiceBuildChatToolRequestBuildsFeatureChatFromCompactContext(t *testing.T) {
	t.Parallel()

	templateProcessor, err := NewStaticPromptRenderer()
	if err != nil {
		t.Fatalf("NewStaticPromptRenderer: %v", err)
	}

	service := &Service{
		cfg: config.LLMConfig{
			Features: config.FeatureConfig{
				Chat: config.FeatureRouteConfig{Model: "gpt"},
			},
		},
		prompts: templateProcessor,
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	request, err := service.BuildChatToolRequest(context.Background(), PromptScope{}, ChatReplyContext{
		SystemHint:     "compact context",
		EarlierSummary: "earlier summary",
		History: []Message{
			TextMessage(RoleUser, "history 1"),
			TextMessage(RoleAssistant, "history 2"),
		},
		UserMessage: TextMessage(RoleUser, "current"),
	}, []ToolDefinition{{Name: "search_history"}}, "tool policy", []Message{TextMessage(RoleTool, "tool result")})
	if err != nil {
		t.Fatalf("BuildChatToolRequest: %v", err)
	}

	if request.Feature != FeatureChat {
		t.Fatalf("unexpected feature: %s", request.Feature)
	}
	if len(request.Tools) != 1 || request.Tools[0].Name != "search_history" {
		t.Fatalf("unexpected tools: %+v", request.Tools)
	}
	messages := request.Messages
	if len(messages) != 5 {
		t.Fatalf("unexpected message count: %d", len(messages))
	}
	if got := messages[0].Text(); !strings.Contains(got, "compact context") {
		t.Fatalf("expected compact system context, got %q", got)
	}
	if got := messages[0].Text(); !strings.Contains(got, "[Earlier conversation summary: earlier summary]") {
		t.Fatalf("expected embedded summary in system message, got %q", got)
	}
	if got := messages[0].Text(); !strings.Contains(got, "tool policy") {
		t.Fatalf("expected tool policy in system message, got %q", got)
	}
	if got := messages[1].Text(); got != "history 1" {
		t.Fatalf("unexpected first history message: %q", got)
	}
	if got := messages[2].Text(); got != "history 2" {
		t.Fatalf("unexpected second history message: %q", got)
	}
	if got := messages[3].Text(); got != "current" {
		t.Fatalf("unexpected current user message: %q", got)
	}
	if got := messages[4].Text(); got != "tool result" {
		t.Fatalf("unexpected extra message: %q", got)
	}
}

func TestServiceBuildChatToolRequestUsesChatScopedPromptAndResolvedPersona(t *testing.T) {
	store := &promptStoreStub{
		global: adminconfig.GlobalSettings{
			CharacterName:            "global",
			Language:                 "Russian",
			Gender:                   "neutral",
			ToneMode:                 "default",
			AllowTeasing:             false,
			DefaultInteractivityMode: adminconfig.InteractivityDisabled,
		},
		chatFound: true,
		chat: adminconfig.ChatSettings{
			CharacterName: "kitsune",
			Language:      "English",
			Gender:        "female",
			ToneMode:      "sharp",
			AllowTeasing:  adminconfig.BoolPointer(true),
		},
		prompts: map[string]string{
			"chat:0": "global name={{.CharacterName}}",
			"chat:5": "chat name={{.CharacterName}} lang={{.Language}} gender={{.Gender}} tone={{.ToneMode}} tease={{.AllowTeasing}} model={{.Model}} context={{.Context}} policy={{.ToolPolicy}}",
		},
	}

	service := &Service{
		cfg: config.LLMConfig{
			Features: config.FeatureConfig{
				Chat: config.FeatureRouteConfig{Model: "gpt"},
			},
		},
		prompts: NewAdminPromptRenderer(adminconfig.NewService(store)),
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	request, err := service.BuildChatToolRequest(context.Background(), PromptScope{ChatID: 5}, ChatReplyContext{
		SystemHint:  "compact context",
		UserMessage: TextMessage(RoleUser, "current"),
	}, nil, "tool policy", nil)
	if err != nil {
		t.Fatalf("BuildChatToolRequest: %v", err)
	}

	if len(request.Messages) != 2 {
		t.Fatalf("unexpected message count: %d", len(request.Messages))
	}
	system := request.Messages[0].Text()
	for _, want := range []string{
		"chat name=kitsune",
		"lang=English",
		"gender=female",
		"tone=sharp",
		"tease=true",
		"model=gpt",
		"context=compact context",
		"policy=tool policy",
	} {
		if !strings.Contains(system, want) {
			t.Fatalf("expected system prompt to contain %q, got %q", want, system)
		}
	}
	if strings.Contains(system, "global name=global") {
		t.Fatalf("expected chat-scoped prompt, got %q", system)
	}
}
