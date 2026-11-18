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

// stubResolver resolves from a fixed per-feature map; unset features and the
// unsetValue sentinel yield "" (the unset state).
type stubResolver struct {
	models map[Feature]string
	err    error
	calls  int
}

const unsetValue = "<<unset>>"

func (r *stubResolver) ResolveModel(_ context.Context, feature Feature, _ int64) (string, error) {
	r.calls++
	if r.err != nil {
		return "", r.err
	}
	if model, ok := r.models[feature]; ok && model != unsetValue {
		return model, nil
	}

	return "", nil
}

func newResolverTestService(t *testing.T, backend *stubBackend, resolver ModelResolver) *Service {
	t.Helper()

	renderer, err := NewStaticPromptRenderer(true)
	if err != nil {
		t.Fatalf("static renderer: %v", err)
	}

	return &Service{
		cfg: config.LLMConfig{
			Features: config.FeatureConfig{
				Chat:      config.FeatureRouteConfig{Model: "route-chat"},
				Summarize: config.FeatureRouteConfig{Model: "route-summary"},
			},
		},
		prompts:  renderer,
		resolver: resolver,
		logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		backend:  backend,
	}
}

func chatServiceResponse(text string) Response {
	return Response{
		Backend: config.LLMBackendOpenAICompat,
		Message: TextMessage(RoleAssistant, text),
	}
}

// TestBuildChatToolRequestUsesTurnModelForWireAndPrompt pins the turn-model
// contract: the resolved model must drive both the wire request and the
// {{.Model}} placeholder, so the model never claims a different identity in
// its own prompt than the one actually addressed.
func TestBuildChatToolRequestUsesTurnModelForWireAndPrompt(t *testing.T) {
	t.Parallel()

	backend := &stubBackend{name: config.LLMBackendOpenAICompat, response: chatServiceResponse("ok")}
	service := newResolverTestService(t, backend, &stubResolver{})

	request, err := service.BuildChatToolRequest(context.Background(), PromptScope{}, ChatReplyContext{
		UserMessage: TextMessage(RoleUser, "hello"),
	}, "turn-model", nil, "policy", nil)
	if err != nil {
		t.Fatalf("BuildChatToolRequest: %v", err)
	}

	if request.Model != "turn-model" {
		t.Fatalf("expected wire model %q, got %q", "turn-model", request.Model)
	}
	system := request.Messages[0].Text()
	if !strings.Contains(system, "turn-model") {
		t.Fatalf("expected prompt to render the turn model, got %q", system)
	}
	if strings.Contains(system, "route-chat") {
		t.Fatalf("route model must not leak into a turn with an override, got %q", system)
	}
}

// TestBuildChatToolRequestEmptyModelKeepsRoutePromptAndUnsetWire is the
// unset-override path: the wire model stays empty (Generate fills the
// environment route) while the prompt renders that route model, exactly as
// before runtime overrides existed.
func TestBuildChatToolRequestEmptyModelKeepsRoutePromptAndUnsetWire(t *testing.T) {
	t.Parallel()

	backend := &stubBackend{name: config.LLMBackendOpenAICompat, response: chatServiceResponse("ok")}
	service := newResolverTestService(t, backend, &stubResolver{})

	request, err := service.BuildChatToolRequest(context.Background(), PromptScope{}, ChatReplyContext{
		UserMessage: TextMessage(RoleUser, "hello"),
	}, "", nil, "policy", nil)
	if err != nil {
		t.Fatalf("BuildChatToolRequest: %v", err)
	}

	if request.Model != "" {
		t.Fatalf("expected unset wire model, got %q", request.Model)
	}
	if system := request.Messages[0].Text(); !strings.Contains(system, "route-chat") {
		t.Fatalf("expected prompt to render the route model, got %q", system)
	}
}

// TestTurnKeepsModelAcrossIterationsAndSettingChanges is the mid-operation
// consistency guarantee: the model is threaded as a resolved value, so once
// the turn started, neither a route change nor a resolver change can alter
// the wire model or the prompt identity of later iterations.
func TestTurnKeepsModelAcrossIterationsAndSettingChanges(t *testing.T) {
	t.Parallel()

	backend := &stubBackend{name: config.LLMBackendOpenAICompat, response: chatServiceResponse("ok")}
	resolver := &stubResolver{models: map[Feature]string{FeatureChat: "ignored-resolver-model"}}
	service := newResolverTestService(t, backend, resolver)

	// First iteration with the original route.
	first, err := service.BuildChatToolRequest(context.Background(), PromptScope{}, ChatReplyContext{
		UserMessage: TextMessage(RoleUser, "hello"),
	}, "turn-model", nil, "policy", nil)
	if err != nil {
		t.Fatalf("first iteration: %v", err)
	}

	// A settings change lands between iterations: the environment route
	// moves and the resolver would now return a different model.
	service.cfg.Features.Chat.Model = "route-chat-new"
	resolver.models[FeatureChat] = "new-resolver-model"

	second, err := service.BuildChatToolRequest(context.Background(), PromptScope{}, ChatReplyContext{
		UserMessage: TextMessage(RoleUser, "hello"),
	}, "turn-model", nil, "policy", nil)
	if err != nil {
		t.Fatalf("second iteration: %v", err)
	}

	for name, request := range map[string]Request{"first": first, "second": second} {
		if request.Model != "turn-model" {
			t.Fatalf("%s iteration: expected wire model to stay %q, got %q", name, "turn-model", request.Model)
		}
		if system := request.Messages[0].Text(); !strings.Contains(system, "turn-model") {
			t.Fatalf("%s iteration: expected prompt to keep rendering %q, got %q", name, "turn-model", system)
		}
	}
}

// TestSummarizeUsesRuntimeOverride covers the global summarize model: an
// override is stamped on the wire request, unset degrades to the environment
// route via Generate, and a resolution failure fails the call instead of
// silently falling back.
func TestSummarizeUsesRuntimeOverride(t *testing.T) {
	t.Parallel()

	t.Run("override stamped", func(t *testing.T) {
		t.Parallel()

		backend := &stubBackend{name: config.LLMBackendOpenAICompat, response: chatServiceResponse("summary")}
		service := newResolverTestService(t, backend, &stubResolver{models: map[Feature]string{FeatureSummarize: "summary-override"}})

		_, _, err := service.Summarize(context.Background(), PromptScope{ChatID: 5}, "text", "")
		if err != nil {
			t.Fatalf("summarize: %v", err)
		}

		if backend.lastRequest.Model != "summary-override" {
			t.Fatalf("expected override on the wire, got %q", backend.lastRequest.Model)
		}
		if backend.lastRequest.Feature != FeatureSummarize {
			t.Fatalf("expected summarize feature, got %s", backend.lastRequest.Feature)
		}
	})

	t.Run("unset falls back to route", func(t *testing.T) {
		t.Parallel()

		backend := &stubBackend{name: config.LLMBackendOpenAICompat, response: chatServiceResponse("summary")}
		service := newResolverTestService(t, backend, &stubResolver{})

		if _, _, err := service.Summarize(context.Background(), PromptScope{}, "text", ""); err != nil {
			t.Fatalf("summarize: %v", err)
		}

		// Generate fills the environment route for an unset model — the
		// exact pre-override behavior.
		if backend.lastRequest.Model != "route-summary" {
			t.Fatalf("expected the environment route on the wire, got %q", backend.lastRequest.Model)
		}
	})

	t.Run("resolution failure propagates", func(t *testing.T) {
		t.Parallel()

		resolutionErr := errors.New("settings store unavailable")
		backend := &stubBackend{name: config.LLMBackendOpenAICompat, response: chatServiceResponse("summary")}
		service := newResolverTestService(t, backend, &stubResolver{err: resolutionErr})

		_, _, err := service.Summarize(context.Background(), PromptScope{}, "text", "")
		if !errors.Is(err, resolutionErr) {
			t.Fatalf("expected resolution error, got %v", err)
		}
		if backend.lastRequest.Feature != "" {
			t.Fatalf("no generation request may be issued on a resolution failure, got feature %s", backend.lastRequest.Feature)
		}
	})
}

// TestHasModelsForChecksRuntimeOverrides extends the startup gate: when a
// resolver is configured, a set global override must also be served by the
// same backend, and a resolution failure fails the check.
func TestHasModelsForChecksRuntimeOverrides(t *testing.T) {
	t.Parallel()

	t.Run("served override passes", func(t *testing.T) {
		t.Parallel()

		backend := &stubBackend{name: config.LLMBackendOpenAICompat, models: []string{"route-chat", "route-summary", "chat-override"}}
		service := newResolverTestService(t, backend, &stubResolver{models: map[Feature]string{FeatureChat: "chat-override"}})

		hasAll, result := service.HasModelsFor(context.Background(), FeatureChat, FeatureSummarize)
		if !hasAll {
			t.Fatalf("expected check to pass, got %v", result)
		}
	})

	t.Run("unserved override fails", func(t *testing.T) {
		t.Parallel()

		backend := &stubBackend{name: config.LLMBackendOpenAICompat, models: []string{"route-chat", "route-summary"}}
		service := newResolverTestService(t, backend, &stubResolver{models: map[Feature]string{FeatureChat: "missing-override"}})

		hasAll, result := service.HasModelsFor(context.Background(), FeatureChat, FeatureSummarize)
		if hasAll {
			t.Fatalf("expected an unserved override to fail the check")
		}
		if result[string(FeatureChat)] {
			t.Fatalf("expected chat to be marked unavailable, got %v", result)
		}
		if !result[string(FeatureSummarize)] {
			t.Fatalf("unset summarize override must not affect its feature, got %v", result)
		}
	})

	t.Run("resolution failure fails the check", func(t *testing.T) {
		t.Parallel()

		backend := &stubBackend{name: config.LLMBackendOpenAICompat, models: []string{"route-chat", "route-summary"}}
		service := newResolverTestService(t, backend, &stubResolver{err: errors.New("settings store unavailable")})

		hasAll, _ := service.HasModelsFor(context.Background(), FeatureChat)
		if hasAll {
			t.Fatalf("expected a resolution failure to fail the check")
		}
	})

	t.Run("no resolver keeps route-only behavior", func(t *testing.T) {
		t.Parallel()

		backend := &stubBackend{name: config.LLMBackendOpenAICompat, models: []string{"route-chat", "route-summary"}}
		service := newResolverTestService(t, backend, nil)

		hasAll, _ := service.HasModelsFor(context.Background(), FeatureChat, FeatureSummarize)
		if !hasAll {
			t.Fatalf("expected route-only check to pass")
		}
	})
}

// TestListModelsSanitizesTransportErrors pins the privacy rule for the admin
// picker path: the backend list feeds UI flows, so a transport error must
// never carry the endpoint URL (which may hold credentials).
func TestListModelsSanitizesTransportErrors(t *testing.T) {
	t.Parallel()

	backend := &stubBackend{
		name:          config.LLMBackendOpenAICompat,
		listModelsErr: errors.New(`Get "http://user:supersecret@vision-host:8080/v1/models": connection refused`),
	}
	service := newResolverTestService(t, backend, nil)

	models, err := service.ListModels(context.Background())
	if err == nil {
		t.Fatal("expected the transport error to surface")
	}
	if models != nil {
		t.Fatalf("expected no models on error, got %v", models)
	}
	if !errors.Is(err, ErrLlmBackendRequestFailed) {
		t.Fatalf("expected typed backend error, got %v", err)
	}
	assertLLMErrorChainClean(t, err, "supersecret", "vision-host")
}
