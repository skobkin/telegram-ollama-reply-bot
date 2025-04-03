package llm

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"telegram-ollama-reply-bot/config"

	"encoding/base64"

	"github.com/getsentry/sentry-go"
	"github.com/sashabaranov/go-openai"
)

var (
	ErrLlmBackendRequestFailed = errors.New("llm back-end request failed")
	ErrNoChoices               = errors.New("no choices in LLM response")
	ErrTemplateProcessing      = errors.New("template processing failed")
)

type LlmConnector struct {
	client            *openai.Client
	cfg               config.LLMConfig
	templateProcessor *TemplateProcessor
}

func NewConnector(cfg config.LLMConfig, templateProcessor *TemplateProcessor) *LlmConnector {
	config := openai.DefaultConfig(cfg.APIToken)
	config.BaseURL = cfg.APIBaseURL

	client := openai.NewClientWithConfig(config)

	return &LlmConnector{
		client:            client,
		cfg:               cfg,
		templateProcessor: templateProcessor,
	}
}

func (l *LlmConnector) HandleChatMessage(userMessage ChatMessage, requestContext RequestContext) (string, error) {
	systemPrompt, err := l.templateProcessor.ProcessChatTemplate(l.cfg.Models.TextRequestModel, requestContext.Prompt())
	if err != nil {
		slog.Error("llm: Template processing failed", "error", err)
		sentry.CaptureException(err)
		return "", ErrTemplateProcessing
	}

	historyLength := len(requestContext.Chat.History)

	if historyLength > 0 {
		systemPrompt += "\nYou have access to last " + strconv.Itoa(historyLength) + " messages in this chat."
	}

	req := openai.ChatCompletionRequest{
		Model: l.cfg.Models.TextRequestModel,
		Messages: []openai.ChatCompletionMessage{
			{
				Role:    openai.ChatMessageRoleSystem,
				Content: systemPrompt,
			},
		},
	}

	if historyLength > 0 {
		for _, msg := range requestContext.Chat.History {
			req.Messages = append(req.Messages, chatMessageToOpenAiChatCompletionMessage(msg))
		}
	}

	req.Messages = append(req.Messages, chatMessageToOpenAiChatCompletionMessage(userMessage))

	resp, err := l.client.CreateChatCompletion(context.Background(), req)
	if err != nil {
		slog.Error("llm: LLM back-end request failed", "error", err)
		sentry.CaptureException(err)

		return "", ErrLlmBackendRequestFailed
	}

	slog.Debug("llm: Received LLM back-end response", "response", resp)

	if len(resp.Choices) < 1 {
		slog.Error("llm: LLM back-end reply has no choices")
		sentry.CaptureMessage("LLM back-end reply has no choices")

		return "", ErrNoChoices
	}

	return resp.Choices[0].Message.Content, nil
}

func (l *LlmConnector) Summarize(text string, instructions string) (string, error) {
	systemPrompt, err := l.templateProcessor.ProcessSummarizeTemplate()
	if err != nil {
		slog.Error("llm: Template processing failed", "error", err)
		sentry.CaptureException(err)
		return "", ErrTemplateProcessing
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

	resp, err := l.client.CreateChatCompletion(context.Background(), req)
	if err != nil {
		slog.Error("llm: LLM back-end request failed", "error", err)
		sentry.CaptureException(err)

		return "", ErrLlmBackendRequestFailed
	}

	slog.Debug("llm: Received LLM back-end response", "response", resp)

	if len(resp.Choices) < 1 {
		slog.Error("llm: LLM back-end reply has no choices")
		sentry.CaptureMessage("LLM back-end reply has no choices")

		return "", ErrNoChoices
	}

	return resp.Choices[0].Message.Content, nil
}

func (l *LlmConnector) HasAllModels(ctx context.Context, models config.ModelSelection) (bool, map[string]bool) {
	modelList, err := l.client.ListModels(ctx)
	if err != nil {
		slog.Error("llm: Model list request failed", "error", err)
		sentry.CaptureException(err)

		return false, map[string]bool{}
	}

	modelIds := []string{models.TextRequestModel, models.SummarizeModel}
	slog.Info("llm: Returned models count", "count", len(modelList.Models))
	slog.Debug("llm: Returned model list", "models", modelList)
	slog.Info("llm: Checking for requested models", "requested", modelIds)

	requestedModelsCount := len(modelIds)
	searchResult := make(map[string]bool, requestedModelsCount)

	for _, modelId := range modelIds {
		searchResult[modelId] = false
	}

	for _, model := range modelList.Models {
		if slices.Contains(modelIds, model.ID) {
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

func (l *LlmConnector) RecognizeImage(imageData []byte) (string, error) {
	systemPrompt, err := l.templateProcessor.ProcessImageRecognitionTemplate()
	if err != nil {
		slog.Error("llm: Template processing failed", "error", err)
		sentry.CaptureException(err)
		return "", ErrTemplateProcessing
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
					//{
					//	Type: openai.ChatMessagePartTypeText,
					//	Text: "What do you see in this image?",
					//},
					{
						Type: openai.ChatMessagePartTypeImageURL,
						ImageURL: &openai.ChatMessageImageURL{
							URL: fmt.Sprintf("data:image/jpeg;base64,%s", base64.StdEncoding.EncodeToString(imageData)),
							//Detail: "auto",
						},
					},
				},
			},
		},
	}

	resp, err := l.client.CreateChatCompletion(context.Background(), req)
	if err != nil {
		slog.Error("llm: LLM back-end request failed", "error", err)
		sentry.CaptureException(err)
		return "", ErrLlmBackendRequestFailed
	}

	if len(resp.Choices) < 1 {
		slog.Error("llm: LLM back-end reply has no choices")
		sentry.CaptureMessage("LLM back-end reply has no choices")
		return "", ErrNoChoices
	}

	return resp.Choices[0].Message.Content, nil
}
