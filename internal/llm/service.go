package llm

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"telegram-ollama-reply-bot/internal/config"
	"telegram-ollama-reply-bot/internal/logging"

	"github.com/getsentry/sentry-go"
)

type Service struct {
	cfg      config.LLMConfig
	prompts  PromptRenderer
	logger   *slog.Logger
	backends map[string]backend
}

func NewService(cfg config.LLMConfig, prompts PromptRenderer, logger *slog.Logger) (*Service, error) {
	backends := make(map[string]backend, 2)

	for _, backendName := range cfg.UsedBackends() {
		var impl backend

		switch backendName {
		case config.LLMBackendOpenAICompat:
			if cfg.Backends.OpenAICompat.BaseURL == "" {
				return nil, errors.Join(ErrBackendNotConfigured, fmt.Errorf("%s base url is empty", backendName))
			}

			impl = newOpenAICompatBackend(cfg.Backends.OpenAICompat, logger.With("backend", backendName))
		case config.LLMBackendOllama:
			if cfg.Backends.Ollama.BaseURL == "" {
				return nil, errors.Join(ErrBackendNotConfigured, fmt.Errorf("%s base url is empty", backendName))
			}

			impl = newOllamaBackend(cfg.Backends.Ollama, logger.With("backend", backendName))
		default:
			return nil, errors.Join(ErrUnknownBackend, fmt.Errorf("backend=%s", backendName))
		}

		backends[backendName] = impl
	}

	return &Service{
		cfg:      cfg,
		prompts:  prompts,
		logger:   logger,
		backends: backends,
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

	impl, ok := s.backends[route.Backend]
	if !ok {
		return Response{}, errors.Join(ErrBackendNotConfigured, fmt.Errorf("backend=%s", route.Backend))
	}

	caps := impl.Capabilities()
	if len(req.Tools) > 0 && !caps.ToolCalls {
		return Response{}, errors.Join(ErrUnsupportedCapability, fmt.Errorf("backend=%s capability=tool_calls", impl.Name()))
	}

	if requestHasImage(req) && !caps.ImageInput {
		return Response{}, errors.Join(ErrUnsupportedCapability, fmt.Errorf("backend=%s capability=image_input", impl.Name()))
	}

	resp, err := impl.Generate(ctx, req)
	if err != nil {
		logger.Error(
			"backend request failed",
			"backend", route.Backend,
			"feature", req.Feature,
			"model", req.Model,
			"error", err,
		)
		sentry.CaptureException(err)

		return Response{}, errors.Join(ErrLlmBackendRequestFailed, err)
	}

	return resp, nil
}

func (s *Service) HandleChatMessage(ctx context.Context, scope PromptScope, requestContext ChatReplyContext) (string, *TokenUsage, error) {
	logger := logging.FromContext(ctx, s.logger)
	route, err := s.cfg.RouteForFeature(string(FeatureChat))
	if err != nil {
		return "", nil, errors.Join(ErrFeatureRouteInvalid, err)
	}

	systemPrompt, err := s.prompts.RenderChatPrompt(ctx, scope, route.Model, requestContext.SystemHint)
	if err != nil {
		logger.Error("chat template processing failed", "error", err)
		sentry.CaptureException(err)

		return "", nil, ErrTemplateProcessing
	}

	messages := []Message{TextMessage(RoleSystem, systemPrompt)}

	if requestContext.EarlierSummary != "" {
		messages = append(messages, TextMessage(RoleSystem, "[Earlier conversation summary: "+requestContext.EarlierSummary+"]"))
	}

	messages = append(messages, requestContext.History...)
	messages = append(messages, requestContext.UserMessage)

	resp, err := s.Generate(ctx, Request{
		Feature:  FeatureChat,
		Messages: messages,
	})
	if err != nil {
		return "", nil, err
	}

	if resp.Message.Text() == "" && len(resp.Message.ToolCalls) == 0 {
		logger.Error("chat completion has no choices", "model", resp.Model, "backend", resp.Backend)
		sentry.CaptureMessage("LLM back-end reply has no choices")

		return "", nil, ErrNoChoices
	}

	usage := resp.Usage

	return resp.Message.Text(), &usage, nil
}

func (s *Service) Summarize(ctx context.Context, scope PromptScope, text string, instructions string) (string, *TokenUsage, error) {
	logger := logging.FromContext(ctx, s.logger)

	systemPrompt, err := s.prompts.RenderSummarizePrompt(ctx, scope)
	if err != nil {
		logger.Error("summarize template processing failed", "error", err)
		sentry.CaptureException(err)

		return "", nil, ErrTemplateProcessing
	}

	if instructions != "" {
		systemPrompt = systemPrompt + "\n\nAdditional instruction from user:\n\n>" + instructions
	}

	resp, err := s.Generate(ctx, Request{
		Feature: FeatureSummarize,
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

func (s *Service) RecognizeImage(ctx context.Context, scope PromptScope, imageData []byte) (string, *TokenUsage, error) {
	logger := logging.FromContext(ctx, s.logger)

	systemPrompt, err := s.prompts.RenderImageRecognitionPrompt(ctx, scope)
	if err != nil {
		logger.Error("image recognition template processing failed", "error", err)
		sentry.CaptureException(err)

		return "", nil, ErrTemplateProcessing
	}

	resp, err := s.Generate(ctx, Request{
		Feature: FeatureImageRecognition,
		Messages: []Message{
			TextMessage(RoleSystem, systemPrompt),
			{
				Role: RoleUser,
				Parts: []Part{
					{
						Type:      PartTypeImage,
						MIMEType:  "image/jpeg",
						ImageData: imageData,
					},
				},
			},
		},
	})
	if err != nil {
		return "", nil, err
	}

	if resp.Message.Text() == "" && len(resp.Message.ToolCalls) == 0 {
		logger.Error("image recognition completion has no choices", "model", resp.Model, "backend", resp.Backend)
		sentry.CaptureMessage("LLM back-end reply has no choices")

		return "", nil, ErrNoChoices
	}

	usage := resp.Usage

	return resp.Message.Text(), &usage, nil
}

func (s *Service) HasAllModels(ctx context.Context) (bool, map[string]bool) {
	logger := logging.FromContext(ctx, s.logger)
	result := make(map[string]bool)

	for _, feature := range []Feature{FeatureChat, FeatureSummarize, FeatureImageRecognition, FeatureToolUse} {
		route, err := s.cfg.RouteForFeature(string(feature))
		if err != nil {
			result[string(feature)] = false

			continue
		}

		if route.Model == "" {
			if feature == FeatureToolUse {
				continue
			}

			result[string(feature)] = false

			continue
		}

		impl, ok := s.backends[route.Backend]
		if !ok {
			result[string(feature)] = false

			continue
		}

		models, err := impl.ListModels(ctx)
		if err != nil {
			logger.Error("model list request failed", "backend", route.Backend, "feature", feature, "error", err)
			sentry.CaptureException(err)
			result[string(feature)] = false

			continue
		}

		result[string(feature)] = slices.Contains(models, route.Model)
	}

	for _, available := range result {
		if !available {
			return false, result
		}
	}

	return true, result
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
