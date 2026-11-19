package llm

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"telegram-ollama-reply-bot/internal/config"
	"telegram-ollama-reply-bot/internal/logging"
	"telegram-ollama-reply-bot/internal/support/safeerr"

	"github.com/getsentry/sentry-go"
)

type Service struct {
	cfg     config.LLMConfig
	prompts PromptRenderer
	// resolver resolves runtime model overrides. Nil keeps the pure
	// environment-route behavior; a resolver may still return "" (unset),
	// which degrades to the same route fallback.
	resolver ModelResolver
	logger   *slog.Logger
	backend  backend
}

// NewService builds a service. The resolver may be nil when the deployment
// runs without runtime model settings.
func NewService(cfg config.LLMConfig, prompts PromptRenderer, resolver ModelResolver, logger *slog.Logger) (*Service, error) {
	if cfg.Backends.OpenAICompat.BaseURL == "" {
		return nil, errors.Join(ErrBackendNotConfigured, fmt.Errorf("%s base url is empty", config.LLMBackendOpenAICompat))
	}

	return &Service{
		cfg:      cfg,
		prompts:  prompts,
		resolver: resolver,
		logger:   logger,
		backend:  newOpenAICompatBackend(cfg.Backends.OpenAICompat, logger.With("backend", config.LLMBackendOpenAICompat)),
	}, nil
}

func (s *Service) Generate(ctx context.Context, req Request) (Response, error) {
	logger := logging.FromContext(ctx, s.logger)

	route, err := s.cfg.RouteForFeature(string(req.Feature))
	if err != nil {
		logger.Error("feature route lookup failed", "feature", req.Feature, "error", err)

		return Response{}, errors.Join(ErrFeatureRouteInvalid, err)
	}

	if req.Model == "" {
		req.Model = route.Model
	}

	caps := s.backend.Capabilities()
	if len(req.Tools) > 0 && !caps.ToolCalls {
		return Response{}, errors.Join(ErrUnsupportedCapability, fmt.Errorf("backend=%s capability=tool_calls", s.backend.Name()))
	}

	if requestHasImage(req) && !caps.ImageInput {
		return Response{}, errors.Join(ErrUnsupportedCapability, fmt.Errorf("backend=%s capability=image_input", s.backend.Name()))
	}

	resp, err := s.backend.Generate(ctx, req)
	if err != nil {
		// Image-recognition backend errors are sanitized before they reach
		// logs or Sentry: raw SDK errors can embed request URLs (which may
		// carry credentials) and provider bodies (which can echo prompts or
		// keys). The sanitized error discards the cause — Sentry traverses
		// error chains — while cancellation/deadline stay matchable via the
		// stdlib context sentinels. The same treatment applies to any
		// request that opted into silent errors (unsolicited proactive
		// turns): their provider bodies can echo chat content and nothing
		// user-initiated is waiting on the details. All other features keep
		// their existing raw diagnostics.
		if req.Feature == FeatureImageRecognition || req.SilentErrors {
			sanitized := safeerr.WrapTransport(ErrLlmBackendRequestFailed, err)
			logger.Error(
				"backend request failed",
				"backend", s.backend.Name(),
				"feature", req.Feature,
				"model", req.Model,
				"error", sanitized,
			)
			sentry.CaptureException(sanitized)

			return Response{}, sanitized
		}

		logger.Error(
			"backend request failed",
			"backend", s.backend.Name(),
			"feature", req.Feature,
			"model", req.Model,
			"error", err,
		)
		sentry.CaptureException(err)

		return Response{}, errors.Join(ErrLlmBackendRequestFailed, err)
	}

	return resp, nil
}

// BuildChatToolRequest assembles one generation request for the chat tool
// loop. generationModel is the turn's resolved model: when set it drives
// both the wire request and the {{.Model}} prompt placeholder, keeping them
// consistent across every iteration of the turn; when empty the environment
// route model renders in the prompt and the wire model stays unset for
// Generate to fill from the route.
func (s *Service) BuildChatToolRequest(
	ctx context.Context,
	scope PromptScope,
	requestContext ChatReplyContext,
	generationModel string,
	tools []ToolDefinition,
	toolPolicy string,
	extraMessages []Message,
) (Request, error) {
	messages, err := s.buildConversationMessages(ctx, scope, requestContext, generationModel, toolPolicy)
	if err != nil {
		return Request{}, err
	}

	messages = append(messages, extraMessages...)

	return Request{
		Feature:  FeatureChat,
		Model:    generationModel,
		Messages: messages,
		Tools:    tools,
	}, nil
}

// Summarize renders the summarize prompt and generates one summary. The
// runtime model override (global summarize model setting) resolves once per
// call; a resolution failure is an error, never a silent route fallback.
func (s *Service) Summarize(ctx context.Context, scope PromptScope, text string, instructions string) (string, *TokenUsage, error) {
	return s.summarize(ctx, scope, text, instructions, false)
}

// SummarizeSilent is Summarize with content-free failure diagnostics: the
// generation request carries SilentErrors, so backend errors are sanitized
// before logging, capture, and return. Unsolicited proactive turns
// summarize history through it — their provider bodies can echo chat
// content.
func (s *Service) SummarizeSilent(ctx context.Context, scope PromptScope, text string, instructions string) (string, *TokenUsage, error) {
	return s.summarize(ctx, scope, text, instructions, true)
}

func (s *Service) summarize(ctx context.Context, scope PromptScope, text string, instructions string, silent bool) (string, *TokenUsage, error) {
	logger := logging.FromContext(ctx, s.logger)

	systemPrompt, err := s.prompts.RenderSummarizeSystemPrompt(ctx, scope)
	if err != nil {
		logger.Error("summarize template processing failed", "error", err)
		sentry.CaptureException(err)

		return "", nil, ErrTemplateProcessing
	}

	if instructions != "" {
		systemPrompt = systemPrompt + "\n\nAdditional instruction from user:\n\n>" + instructions
	}

	model := ""
	if s.resolver != nil {
		model, err = s.resolver.ResolveModel(ctx, FeatureSummarize, scope.ChatID)
		if err != nil {
			logger.Error("summarize model resolution failed", "error", err)
			sentry.CaptureException(err)

			return "", nil, err
		}
	}

	resp, err := s.Generate(ctx, Request{
		Feature:      FeatureSummarize,
		Model:        model,
		SilentErrors: silent,
		Messages: []Message{
			TextMessage(RoleSystem, systemPrompt),
			TextMessage(RoleUser, text),
		},
	})
	if err != nil {
		return "", nil, err
	}

	if resp.Message.Text() == "" && len(resp.Message.ToolCalls) == 0 {
		logger.Error("summarize completion has no choices", "model", resp.Model, "backend", resp.Backend)
		sentry.CaptureMessage("LLM back-end reply has no choices")

		return "", nil, ErrNoChoices
	}

	usage := resp.Usage

	return resp.Message.Text(), &usage, nil
}

// HasModelsFor reports whether every given feature's effective model is
// offered by the backend, along with a per-feature result map. The effective
// model is what the feature would actually request: the global runtime
// override when one is set, otherwise the environment route. A used
// override replaces the route as the validated model — an obsolete route
// model the backend no longer serves must not block startup while the
// persisted selection works, so its absence is reported as a warning. An
// unset override validates the route: resetting to an unavailable fallback
// fails the check loudly, and nothing silently selects another model.
// Unset and resolution failure stay distinct — an unverifiable feature
// fails the check. Callers decide which features to gate on: the startup
// check passes chat and summarize always, and image_recognition only for a
// model-routed recognizer on its actual connection.
func (s *Service) HasModelsFor(ctx context.Context, features ...Feature) (bool, map[string]bool) {
	logger := logging.FromContext(ctx, s.logger)
	result := make(map[string]bool)

	for _, feature := range features {
		route, err := s.cfg.RouteForFeature(string(feature))
		if err != nil {
			result[string(feature)] = false

			continue
		}

		if route.Model == "" {
			result[string(feature)] = false

			continue
		}

		models, err := s.backend.ListModels(ctx)
		if err != nil {
			// Image-feature model-check failures are sanitized before the log
			// and Sentry: raw transport errors carry the endpoint URL, which
			// can hold credentials on the image-only connection. The cause is
			// discarded; other features keep their existing diagnostics.
			logErr := err
			if feature == FeatureImageRecognition {
				logErr = safeerr.WrapTransport(ErrLlmBackendRequestFailed, err)
			}

			logger.Error("model list request failed", "backend", s.backend.Name(), "feature", feature, "error", logErr)
			sentry.CaptureException(logErr)
			result[string(feature)] = false

			continue
		}

		// The chatID is 0 because the startup check covers global defaults
		// only; per-chat overrides are runtime concerns verified when used.
		override := ""
		if s.resolver != nil {
			override, err = s.resolver.ResolveModel(ctx, feature, 0)
			if err != nil {
				logger.Error("runtime model resolution failed", "feature", feature, "error", err)
				sentry.CaptureException(err)
				result[string(feature)] = false

				continue
			}
		}

		if override == "" {
			// The route is the model the feature would request: validate it.
			result[string(feature)] = slices.Contains(models, route.Model)

			continue
		}

		// A set override is the model the feature requests. The route stays
		// only as the documented fallback: unused, its absence may not block
		// startup, but it is worth a warning.
		result[string(feature)] = slices.Contains(models, override)
		if !slices.Contains(models, route.Model) {
			logger.Warn("environment fallback model is not offered by the backend; the runtime override is unaffected",
				"feature", feature, "model", route.Model)
		}
	}

	for _, available := range result {
		if !available {
			return false, result
		}
	}

	return true, result
}

// ListModels returns the backend's served model list, used by the admin UI
// model pickers. Transport errors are sanitized before returning: the raw
// SDK error can embed the endpoint URL, which may carry credentials, and
// the list feeds UI flows that must never surface it.
func (s *Service) ListModels(ctx context.Context) ([]string, error) {
	models, err := s.backend.ListModels(ctx)
	if err != nil {
		return nil, safeerr.WrapTransport(ErrLlmBackendRequestFailed, err)
	}

	return models, nil
}

func (s *Service) buildConversationMessages(ctx context.Context, scope PromptScope, requestContext ChatReplyContext, generationModel string, toolPolicy string) ([]Message, error) {
	logger := logging.FromContext(ctx, s.logger)
	route, err := s.cfg.RouteForFeature(string(FeatureChat))
	if err != nil {
		return nil, errors.Join(ErrFeatureRouteInvalid, err)
	}

	model := generationModel
	if model == "" {
		model = route.Model
	}

	systemPrompt, err := s.prompts.RenderChatSystemPrompt(ctx, scope, model, requestContext.SystemHint, toolPolicy)
	if err != nil {
		logger.Error("chat template processing failed", "error", err)
		sentry.CaptureException(err)

		return nil, ErrTemplateProcessing
	}

	if requestContext.EarlierSummary != "" {
		systemPrompt += "\n\n[Earlier conversation summary: " + requestContext.EarlierSummary + "]"
	}

	messages := []Message{TextMessage(RoleSystem, systemPrompt)}
	messages = append(messages, requestContext.History...)
	messages = append(messages, requestContext.UserMessage)

	return messages, nil
}

func requestHasImage(req Request) bool {
	for _, message := range req.Messages {
		for _, part := range message.Parts {
			if part.Type == PartTypeImage {
				return true
			}
		}
	}

	return false
}
