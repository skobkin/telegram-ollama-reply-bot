package llm

import (
	"context"
	"errors"
	"github.com/getsentry/sentry-go"
	"github.com/sashabaranov/go-openai"
	"log/slog"
	"slices"
	"strconv"
)

var (
	ErrLlmBackendRequestFailed = errors.New("llm back-end request failed")
	ErrNoChoices               = errors.New("no choices in LLM response")
)

type LlmConnector struct {
	client *openai.Client
}

func NewConnector(baseUrl string, token string) *LlmConnector {
	config := openai.DefaultConfig(token)
	config.BaseURL = baseUrl

	client := openai.NewClientWithConfig(config)

	return &LlmConnector{
		client: client,
	}
}

func (l *LlmConnector) HandleChatMessage(userMessage ChatMessage, model string, requestContext RequestContext) (string, error) {
	systemPrompt := "You're a bot in the Telegram chat.\n" +
		"You're using a free model called \"" + model + "\".\n\n" +
		requestContext.Prompt()

	historyLength := len(requestContext.Chat.History)

	if historyLength > 0 {
		systemPrompt += "\nYou have access to last " + strconv.Itoa(historyLength) + " messages in this chat."
	}

	req := openai.ChatCompletionRequest{
		Model: model,
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

func (l *LlmConnector) Summarize(text string, model string, instructions string) (string, error) {
	systemPrompt := "You're a text shortener. Give a VERY SHORT summary as a list of facts. " +
		"Format it like this:\n" +
		"```\n" +
		"- Fact 1\n" +
		"- Fact 2\n\n" +
		"Your short conclusion." +
		"```\n" +
		"Avoid any commentaries and value judgement on the matter unless asked by the user. " +
		"Write the summary **in Russian** unless other instructions provided." +
		"Avoid using ANY formatting in the text except simple \"-\" for each fact even if asked to.\n\n" +
		"Limit the summary to maximum of 2000 characters. Avoid exceeding it at any cost. Be as brief as possible."

	if instructions != "" {
		systemPrompt = systemPrompt + "\n\nAdditional instruction from user:\n\n>" + instructions
	}

	req := openai.ChatCompletionRequest{
		Model: model,
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

	slog.Debug("llm: Received LLM back-end response", resp)

	if len(resp.Choices) < 1 {
		slog.Error("llm: LLM back-end reply has no choices")
		sentry.CaptureMessage("LLM back-end reply has no choices")

		return "", ErrNoChoices
	}

	return resp.Choices[0].Message.Content, nil
}

func (l *LlmConnector) HasAllModels(modelIds []string) (bool, map[string]bool) {
	modelList, err := l.client.ListModels(context.Background())
	if err != nil {
		slog.Error("llm: Model list request failed", "error", err)
		sentry.CaptureException(err)

		return false, map[string]bool{}
	}

	slog.Info("llm: Returned model list", "models", modelList)
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
