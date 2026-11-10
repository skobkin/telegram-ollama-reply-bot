package llm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
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
				Chat:             config.FeatureRouteConfig{Model: "chat-model"},
				Summarize:        config.FeatureRouteConfig{Model: "summary-model"},
				ImageRecognition: config.FeatureRouteConfig{Model: "vision-model"},
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

	if backendStub.lastRequest.Model != "summary-model" {
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

	if backendStub.lastRequest.Model != "chat-model" {
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

	templateProcessor, err := NewStaticPromptRenderer(true)
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
		prompts: NewAdminPromptRenderer(adminconfig.NewService(store), true),
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

// captureLogger returns a logger writing to a buffer, for asserting that
// logged errors are sanitized.
func captureLogger() (*slog.Logger, *strings.Builder) {
	buf := &strings.Builder{}

	return slog.New(slog.NewTextHandler(buf, nil)), buf
}

// TestServiceGenerateSanitizesImageFeatureErrors is the privacy regression
// for the image-recognition route: raw backend errors can embed request URLs
// (with credentials) and provider bodies (echoing prompts or keys), so the
// error logged and captured for Sentry — and the error returned to vision —
// must be the sanitized typed one, with cancellation still matchable.
func TestServiceGenerateSanitizesImageFeatureErrors(t *testing.T) {
	t.Parallel()

	newService := func(generateErr error, logger *slog.Logger) *Service {
		return &Service{
			cfg: config.LLMConfig{
				Features: config.FeatureConfig{
					Chat:             config.FeatureRouteConfig{Model: "m"},
					Summarize:        config.FeatureRouteConfig{Model: "m"},
					ImageRecognition: config.FeatureRouteConfig{Model: "m"},
				},
			},
			logger: logger,
			backend: &stubBackend{
				name:        config.LLMBackendOpenAICompat,
				caps:        Capabilities{ToolCalls: true, ImageInput: true, ModelListing: true},
				generateErr: generateErr,
			},
		}
	}

	imageRequest := Request{
		Feature:  FeatureImageRecognition,
		Messages: []Message{TextMessage(RoleUser, "describe")},
	}

	t.Run("unknown error discards cause", func(t *testing.T) {
		t.Parallel()

		rawErr := &url.Error{Op: "Post", URL: "http://llm.internal/v1?key=sk-TESTSECRET", Err: errors.New("server blew up")}

		logger, logBuf := captureLogger()
		_, err := newService(rawErr, logger).Generate(context.Background(), imageRequest)

		if !errors.Is(err, ErrLlmBackendRequestFailed) {
			t.Fatalf("expected typed kind, got %v", err)
		}
		assertLLMErrorChainClean(t, err, "TESTSECRET", "server blew up")
		if strings.Contains(logBuf.String(), "TESTSECRET") || strings.Contains(logBuf.String(), "server blew up") {
			t.Fatalf("logged error leaks raw cause: %q", logBuf.String())
		}
	})

	t.Run("cancellation stays matchable", func(t *testing.T) {
		t.Parallel()

		rawErr := fmt.Errorf("post failed: %w", context.Canceled)

		logger, logBuf := captureLogger()
		_, err := newService(rawErr, logger).Generate(context.Background(), imageRequest)

		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled matchable, got %v", err)
		}
		if !errors.Is(err, ErrLlmBackendRequestFailed) {
			t.Fatalf("expected typed kind, got %v", err)
		}
		if strings.Contains(logBuf.String(), "post failed") {
			t.Fatalf("logged error leaks raw cause: %q", logBuf.String())
		}
	})

	t.Run("chat keeps raw diagnostics", func(t *testing.T) {
		t.Parallel()

		rawErr := errors.New("chat backend says sk-TESTSECRET")

		logger, logBuf := captureLogger()
		_, err := newService(rawErr, logger).Generate(context.Background(), Request{
			Feature:  FeatureChat,
			Messages: []Message{TextMessage(RoleUser, "hi")},
		})

		if !strings.Contains(err.Error(), "sk-TESTSECRET") {
			t.Fatalf("chat errors must keep today's raw surface, got %v", err)
		}
		if !strings.Contains(logBuf.String(), "sk-TESTSECRET") {
			t.Fatalf("chat logging must stay unchanged, got %q", logBuf.String())
		}
	})
}

// TestServiceHasModelsForFeatureSubset checks the variadic gate: an image
// model missing from the backend only fails the boot when image_recognition
// is actually requested.
func TestServiceHasModelsForFeatureSubset(t *testing.T) {
	t.Parallel()

	service := &Service{
		cfg: config.LLMConfig{
			Features: config.FeatureConfig{
				Chat:             config.FeatureRouteConfig{Model: "chat-model"},
				Summarize:        config.FeatureRouteConfig{Model: "summary-model"},
				ImageRecognition: config.FeatureRouteConfig{Model: "image-model"},
			},
		},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		backend: &stubBackend{
			name:   config.LLMBackendOpenAICompat,
			models: []string{"chat-model", "summary-model"},
		},
	}

	hasAll, result := service.HasModelsFor(context.Background(), FeatureChat, FeatureSummarize)
	if !hasAll {
		t.Fatalf("expected subset check to pass without the image model, got %v", result)
	}

	hasAll, result = service.HasModelsFor(context.Background(), FeatureChat, FeatureSummarize, FeatureImageRecognition)
	if hasAll {
		t.Fatalf("expected failure when the image model is requested but missing")
	}
	if result[string(FeatureImageRecognition)] {
		t.Fatalf("expected image_recognition marked unavailable, got %v", result)
	}
}

// TestServiceHasModelsForSanitizesImageListErrors extends the privacy rule to
// the startup model check: an image-feature ListModels failure must log a
// sanitized error (the endpoint URL can carry credentials), while chat keeps
// its existing raw diagnostics.
func TestServiceHasModelsForSanitizesImageListErrors(t *testing.T) {
	t.Parallel()

	newService := func(logger *slog.Logger) *Service {
		return &Service{
			cfg: config.LLMConfig{
				Features: config.FeatureConfig{
					Chat:             config.FeatureRouteConfig{Model: "m"},
					Summarize:        config.FeatureRouteConfig{Model: "m"},
					ImageRecognition: config.FeatureRouteConfig{Model: "m"},
				},
			},
			logger: logger,
			backend: &stubBackend{
				name:          config.LLMBackendOpenAICompat,
				listModelsErr: &url.Error{Op: "Get", URL: "http://image.internal/v1?token=sk-TESTSECRET", Err: errors.New("boom")},
			},
		}
	}

	t.Run("image feature sanitized", func(t *testing.T) {
		t.Parallel()

		logger, logBuf := captureLogger()
		hasAll, _ := newService(logger).HasModelsFor(context.Background(), FeatureImageRecognition)

		if hasAll {
			t.Fatalf("expected check to fail on list error")
		}
		if strings.Contains(logBuf.String(), "TESTSECRET") {
			t.Fatalf("logged model-check error leaks endpoint: %q", logBuf.String())
		}
	})

	t.Run("chat feature keeps raw diagnostics", func(t *testing.T) {
		t.Parallel()

		logger, logBuf := captureLogger()
		_, _ = newService(logger).HasModelsFor(context.Background(), FeatureChat)

		if !strings.Contains(logBuf.String(), "TESTSECRET") {
			t.Fatalf("chat model-check logging must stay unchanged, got %q", logBuf.String())
		}
	})
}

// assertLLMErrorChainClean recursively walks the error chain (single and
// multi Unwrap) — the traversal Sentry's exception chain performs — and fails
// when any node's message contains one of the sentinels.
func assertLLMErrorChainClean(t *testing.T, err error, sentinels ...string) {
	t.Helper()

	seen := make(map[error]bool)

	var visit func(error)
	visit = func(e error) {
		if e == nil || seen[e] {
			return
		}
		seen[e] = true

		msg := e.Error()
		for _, s := range sentinels {
			if strings.Contains(msg, s) {
				t.Fatalf("error chain leaks %q: %q", s, msg)
			}
		}

		//nolint:errorlint // intentional: enumerating the direct unwrap structure like Sentry, not matching a target type
		switch unwrapper := e.(type) {
		case interface{ Unwrap() error }:
			visit(unwrapper.Unwrap())
		case interface{ Unwrap() []error }:
			for _, child := range unwrapper.Unwrap() {
				visit(child)
			}
		}
	}

	visit(err)
}
