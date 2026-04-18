package llm

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"telegram-ollama-reply-bot/internal/config"
	"telegram-ollama-reply-bot/internal/logging"

	"encoding/base64"

	"log/slog"

	"github.com/getsentry/sentry-go"
	"github.com/sashabaranov/go-openai"
)

var (
	ErrLlmBackendRequestFailed = errors.New("llm back-end request failed")
	ErrNoChoices               = errors.New("no choices in LLM response")
	ErrTemplateProcessing      = errors.New("template processing failed")
)

type Connector struct {
	client            *openai.Client
	cfg               config.LLMConfig
	templateProcessor *TemplateProcessor
	logger            *slog.Logger
}

type TokenUsage struct {
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	Cost             float64
}

func NewConnector(cfg config.LLMConfig, templateProcessor *TemplateProcessor, logger *slog.Logger) *Connector {
	clientCfg := openai.DefaultConfig(cfg.APIToken)
	clientCfg.BaseURL = cfg.APIBaseURL

	client := openai.NewClientWithConfig(clientCfg)

	return &Connector{
		client:            client,
		cfg:               cfg,
		templateProcessor: templateProcessor,
		logger:            logger,
	}
}

func (l *Connector) HandleChatMessage(ctx context.Context, userMessage ChatMessage, requestContext RequestContext) (string, *TokenUsage, error) {
	logger := logging.FromContext(ctx, l.logger)

	systemPrompt, err := l.templateProcessor.ProcessChatTemplate(l.cfg.Models.TextRequestModel, requestContext.Prompt())
	if err != nil {
		logger.Error("chat template processing failed", "error", err)
		sentry.CaptureException(err)

		return "", nil, ErrTemplateProcessing
	}

	history := requestContext.Chat.History
	earlierSummary := requestContext.Chat.EarlierSummary

	req := openai.ChatCompletionRequest{
		Model: l.cfg.Models.TextRequestModel,
		Messages: []openai.ChatCompletionMessage{
			{
				Role:    openai.ChatMessageRoleSystem,
				Content: systemPrompt,
			},
		},
	}

	if earlierSummary != "" {
		req.Messages = append(req.Messages, openai.ChatCompletionMessage{
			Role:    openai.ChatMessageRoleSystem,
			Content: "[Earlier conversation summary: " + earlierSummary + "]",
		})
	}

	if len(history) > 0 {
		for _, msg := range history {
			req.Messages = append(req.Messages, chatMessageToOpenAiChatCompletionMessage(msg))
		}
	}

	req.Messages = append(req.Messages, chatMessageToOpenAiChatCompletionMessage(userMessage))

	resp, err := l.client.CreateChatCompletion(ctx, req)
	if err != nil {
		logger.Error("chat completion request failed", "error", err, "model", req.Model, "history_messages", len(history))
		sentry.CaptureException(err)

		return "", nil, errors.Join(ErrLlmBackendRequestFailed, err)
	}

	logger.Debug(
		"chat completion received",
		"model", req.Model,
		"choices", len(resp.Choices),
		"prompt_tokens", resp.Usage.PromptTokens,
		"completion_tokens", resp.Usage.CompletionTokens,
		"total_tokens", resp.Usage.TotalTokens,
	)

	if len(resp.Choices) < 1 {
		logger.Error("chat completion has no choices", "model", req.Model)
		sentry.CaptureMessage("LLM back-end reply has no choices")

		return "", nil, ErrNoChoices
	}

	usage := &TokenUsage{
		PromptTokens:     resp.Usage.PromptTokens,
		CompletionTokens: resp.Usage.CompletionTokens,
		TotalTokens:      resp.Usage.TotalTokens,
	}

	return resp.Choices[0].Message.Content, usage, nil
}

func (l *Connector) Summarize(ctx context.Context, text string, instructions string) (string, *TokenUsage, error) {
	logger := logging.FromContext(ctx, l.logger)

	systemPrompt, err := l.templateProcessor.ProcessSummarizeTemplate()
	if err != nil {
		logger.Error("summarize template processing failed", "error", err)
		sentry.CaptureException(err)

		return "", nil, ErrTemplateProcessing
	}

	if instructions != "" {
		systemPrompt = systemPrompt + "\n\nAdditional instruction from user:\n\n>" + instructions
	}

	req := openai.ChatCompletionRequest{
		Model: l.cfg.Models.SummarizeModel,
		Messages: []openai.ChatCompletionMessage{
			{
				Role:    openai.ChatMessageRoleSystem,
				Content: systemPrompt,
			},
		},
	}

	req.Messages = append(req.Messages, openai.ChatCompletionMessage{
		Role:    openai.ChatMessageRoleUser,
		Content: text,
	})

	resp, err := l.client.CreateChatCompletion(ctx, req)
	if err != nil {
		logger.Error("summarize request failed", "error", err, "model", req.Model, "text_length", len(text), "has_instructions", instructions != "")
		sentry.CaptureException(err)

		return "", nil, errors.Join(ErrLlmBackendRequestFailed, err)
	}

	logger.Debug(
		"summarize completion received",
		"model", req.Model,
		"choices", len(resp.Choices),
		"prompt_tokens", resp.Usage.PromptTokens,
		"completion_tokens", resp.Usage.CompletionTokens,
		"total_tokens", resp.Usage.TotalTokens,
	)

	if len(resp.Choices) < 1 {
		logger.Error("summarize completion has no choices", "model", req.Model)
		sentry.CaptureMessage("LLM back-end reply has no choices")

		return "", nil, ErrNoChoices
	}

	usage := &TokenUsage{
		PromptTokens:     resp.Usage.PromptTokens,
		CompletionTokens: resp.Usage.CompletionTokens,
		TotalTokens:      resp.Usage.TotalTokens,
	}

	return resp.Choices[0].Message.Content, usage, nil
}

func (l *Connector) HasAllModels(ctx context.Context, models config.ModelSelection) (bool, map[string]bool) {
	logger := logging.FromContext(ctx, l.logger)

	modelList, err := l.client.ListModels(ctx)
	if err != nil {
		logger.Error("model list request failed", "error", err)
		sentry.CaptureException(err)

		return false, map[string]bool{}
	}

	modelIDs := []string{models.TextRequestModel, models.SummarizeModel}
	logger.Info("received model list", "count", len(modelList.Models))
	logger.Debug("checking requested models", "requested", modelIDs)

	requestedModelsCount := len(modelIDs)
	searchResult := make(map[string]bool, requestedModelsCount)

	for _, modelID := range modelIDs {
		searchResult[modelID] = false
	}

	for _, model := range modelList.Models {
		if slices.Contains(modelIDs, model.ID) {
			searchResult[model.ID] = true
		}
	}

	for _, v := range searchResult {
		if !v {
			return false, searchResult
		}
	}

	return true, searchResult
}

func (l *Connector) RecognizeImage(ctx context.Context, imageData []byte) (string, *TokenUsage, error) {
	logger := logging.FromContext(ctx, l.logger)

	systemPrompt, err := l.templateProcessor.ProcessImageRecognitionTemplate()
	if err != nil {
		logger.Error("image recognition template processing failed", "error", err)
		sentry.CaptureException(err)

		return "", nil, ErrTemplateProcessing
	}

	req := openai.ChatCompletionRequest{
		Model: l.cfg.Models.ImageRecognitionModel,
		Messages: []openai.ChatCompletionMessage{
			{
				Role:    openai.ChatMessageRoleSystem,
				Content: systemPrompt,
			},
			{
				Role: openai.ChatMessageRoleUser,
				MultiContent: []openai.ChatMessagePart{
					// {
					// 	Type: openai.ChatMessagePartTypeText,
					// 	Text: "What do you see in this image?",
					// },
					{
						Type: openai.ChatMessagePartTypeImageURL,
						ImageURL: &openai.ChatMessageImageURL{
							URL: fmt.Sprintf("data:image/jpeg;base64,%s", base64.StdEncoding.EncodeToString(imageData)),
							// Detail: "auto",
						},
					},
				},
			},
		},
	}

	resp, err := l.client.CreateChatCompletion(ctx, req)
	if err != nil {
		logger.Error("image recognition request failed", "error", err, "model", req.Model, "image_bytes", len(imageData))
		sentry.CaptureException(err)

		return "", nil, errors.Join(ErrLlmBackendRequestFailed, err)
	}

	if len(resp.Choices) < 1 {
		logger.Error("image recognition completion has no choices", "model", req.Model)
		sentry.CaptureMessage("LLM back-end reply has no choices")

		return "", nil, ErrNoChoices
	}

	usage := &TokenUsage{
		PromptTokens:     resp.Usage.PromptTokens,
		CompletionTokens: resp.Usage.CompletionTokens,
		TotalTokens:      resp.Usage.TotalTokens,
	}

	return resp.Choices[0].Message.Content, usage, nil
}
