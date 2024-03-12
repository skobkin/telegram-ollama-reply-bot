package llm

import (
	"context"
	"errors"
	"github.com/sashabaranov/go-openai"
	"log/slog"
)

var (
	ErrLlmBackendRequestFailed = errors.New("llm back-end request failed")
	ErrNoChoices               = errors.New("no choices in LLM response")

	ModelMistralUncensored = "dolphin-mistral"
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

func (l *LlmConnector) HandleSingleRequest(text string, model string, requestContext RequestContext) (string, error) {
	req := openai.ChatCompletionRequest{
		Model: model,
		Messages: []openai.ChatCompletionMessage{
			{
				Role: openai.ChatMessageRoleSystem,
				Content: "You're a bot in the Telegram chat. " +
					"You're using a free model called \"" + model + "\". " +
					"You see only messages addressed to you using commands due to privacy settings. " +
					requestContext.Prompt(),
			},
		},
	}

	req.Messages = append(req.Messages, openai.ChatCompletionMessage{
		Role:    openai.ChatMessageRoleUser,
		Content: text,
	})

	resp, err := l.client.CreateChatCompletion(context.Background(), req)
	if err != nil {
		slog.Error("LLM back-end request failed", err)

		return "", ErrLlmBackendRequestFailed
	}

	slog.Debug("Received LLM back-end response", resp)

	if len(resp.Choices) < 1 {
		slog.Error("LLM back-end reply has no choices")

		return "", ErrNoChoices
	}

	return resp.Choices[0].Message.Content, nil
}

func (l *LlmConnector) Summarize(text string, model string) (string, error) {
	req := openai.ChatCompletionRequest{
		Model: model,
		Messages: []openai.ChatCompletionMessage{
			{
				Role: openai.ChatMessageRoleSystem,
				Content: "You are a short digest editor. Summarize the text you received " +
					"as a list of bullet points with most important facts from the text. " +
					"If possible, use the same language as the original text.",
			},
		},
	}

	req.Messages = append(req.Messages, openai.ChatCompletionMessage{
		Role:    openai.ChatMessageRoleUser,
		Content: text,
	})

	resp, err := l.client.CreateChatCompletion(context.Background(), req)
	if err != nil {
		slog.Error("LLM back-end request failed", err)

		return "", ErrLlmBackendRequestFailed
	}

	slog.Debug("Received LLM back-end response", resp)

	if len(resp.Choices) < 1 {
		slog.Error("LLM back-end reply has no choices")

		return "", ErrNoChoices
	}

	return resp.Choices[0].Message.Content, nil
}
