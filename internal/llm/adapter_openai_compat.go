package llm

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"telegram-ollama-reply-bot/internal/config"

	"github.com/sashabaranov/go-openai"
)

type openAICompatBackend struct {
	client *openai.Client
	logger *slog.Logger
}

func newOpenAICompatBackend(cfg config.OpenAICompatBackendConfig, logger *slog.Logger) backend {
	clientCfg := openai.DefaultConfig(cfg.APIToken)
	clientCfg.BaseURL = cfg.BaseURL

	return &openAICompatBackend{
		client: openai.NewClientWithConfig(clientCfg),
		logger: logger,
	}
}

func (b *openAICompatBackend) Name() string {
	return config.LLMBackendOpenAICompat
}

func (b *openAICompatBackend) Capabilities() Capabilities {
	return Capabilities{
		ToolCalls:    true,
		ImageInput:   true,
		ModelListing: true,
	}
}

func (b *openAICompatBackend) Generate(ctx context.Context, req Request) (Response, error) {
	chatReq := openai.ChatCompletionRequest{
		Model: req.Model,
	}

	for _, message := range req.Messages {
		chatReq.Messages = append(chatReq.Messages, openAIMessageFromMessage(message))
	}

	if len(req.Tools) > 0 {
		chatReq.Tools = make([]openai.Tool, 0, len(req.Tools))
		for _, tool := range req.Tools {
			chatReq.Tools = append(chatReq.Tools, openai.Tool{
				Type: openai.ToolTypeFunction,
				Function: &openai.FunctionDefinition{
					Name:        tool.Name,
					Description: tool.Description,
					Parameters:  tool.Parameters,
				},
			})
		}
	}

	resp, err := b.client.CreateChatCompletion(ctx, chatReq)
	if err != nil {
		return Response{}, err
	}

	if len(resp.Choices) < 1 {
		return Response{}, ErrNoChoices
	}

	choice := resp.Choices[0]

	return Response{
		Backend:      b.Name(),
		Model:        resp.Model,
		FinishReason: finishReasonFromOpenAI(string(choice.FinishReason)),
		Message:      messageFromOpenAIMessage(choice.Message),
		Usage: TokenUsage{
			PromptTokens:     resp.Usage.PromptTokens,
			CompletionTokens: resp.Usage.CompletionTokens,
			TotalTokens:      resp.Usage.TotalTokens,
		},
	}, nil
}

func (b *openAICompatBackend) ListModels(ctx context.Context) ([]string, error) {
	resp, err := b.client.ListModels(ctx)
	if err != nil {
		return nil, err
	}

	models := make([]string, 0, len(resp.Models))
	for _, model := range resp.Models {
		models = append(models, model.ID)
	}

	return models, nil
}

func openAIMessageFromMessage(message Message) openai.ChatCompletionMessage {
	result := openai.ChatCompletionMessage{
		Role:       string(message.Role),
		Name:       message.Name,
		ToolCallID: message.ToolCallID,
	}

	hasImage := false
	for _, part := range message.Parts {
		if part.Type == PartTypeImage {
			hasImage = true

			break
		}
	}

	if hasImage {
		for _, part := range message.Parts {
			switch part.Type {
			case PartTypeText:
				result.MultiContent = append(result.MultiContent, openai.ChatMessagePart{
					Type: openai.ChatMessagePartTypeText,
					Text: part.Text,
				})
			case PartTypeImage:
				mimeType := part.MIMEType
				if mimeType == "" {
					mimeType = "image/jpeg"
				}

				result.MultiContent = append(result.MultiContent, openai.ChatMessagePart{
					Type: openai.ChatMessagePartTypeImageURL,
					ImageURL: &openai.ChatMessageImageURL{
						URL: fmt.Sprintf("data:%s;base64,%s", mimeType, base64.StdEncoding.EncodeToString(part.ImageData)),
					},
				})
			}
		}
	} else {
		result.Content = message.Text()
	}

	if len(message.ToolCalls) > 0 {
		result.ToolCalls = make([]openai.ToolCall, 0, len(message.ToolCalls))
		for _, call := range message.ToolCalls {
			result.ToolCalls = append(result.ToolCalls, openai.ToolCall{
				ID:   call.ID,
				Type: openai.ToolTypeFunction,
				Function: openai.FunctionCall{
					Name:      call.Name,
					Arguments: string(call.Arguments),
				},
			})
		}
	}

	return result
}

func messageFromOpenAIMessage(message openai.ChatCompletionMessage) Message {
	result := Message{
		Role:       Role(message.Role),
		Name:       message.Name,
		ToolCallID: message.ToolCallID,
	}

	if message.Content != "" {
		result.Parts = append(result.Parts, Part{Type: PartTypeText, Text: message.Content})
	}

	for _, part := range message.MultiContent {
		if part.Type == openai.ChatMessagePartTypeText {
			result.Parts = append(result.Parts, Part{Type: PartTypeText, Text: part.Text})
		}
	}

	if len(message.ToolCalls) > 0 {
		result.ToolCalls = make([]ToolCall, 0, len(message.ToolCalls))
		for _, call := range message.ToolCalls {
			result.ToolCalls = append(result.ToolCalls, ToolCall{
				ID:        call.ID,
				Name:      call.Function.Name,
				Arguments: json.RawMessage(call.Function.Arguments),
			})
		}
	}

	return result
}

func finishReasonFromOpenAI(reason string) FinishReason {
	if reason == "tool_calls" {
		return FinishReasonToolCalls
	}

	return FinishReasonStop
}
