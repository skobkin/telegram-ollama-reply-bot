package llm

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"telegram-ollama-reply-bot/internal/config"
)

type ollamaBackend struct {
	baseURL string
	client  *http.Client
	logger  *slog.Logger
}

type ollamaChatRequest struct {
	Model    string             `json:"model"`
	Messages []ollamaMessage    `json:"messages"`
	Stream   bool               `json:"stream"`
	Tools    []ollamaToolSchema `json:"tools,omitempty"`
}

type ollamaMessage struct {
	Role       string           `json:"role"`
	Content    string           `json:"content,omitempty"`
	Images     []string         `json:"images,omitempty"`
	Name       string           `json:"name,omitempty"`
	ToolCalls  []ollamaToolCall `json:"tool_calls,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
}

type ollamaToolSchema struct {
	Type     string               `json:"type"`
	Function ollamaToolDefinition `json:"function"`
}

type ollamaToolDefinition struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
}

type ollamaToolCall struct {
	Function ollamaToolCallFunction `json:"function"`
}

type ollamaToolCallFunction struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type ollamaChatResponse struct {
	Model      string        `json:"model"`
	Message    ollamaMessage `json:"message"`
	DoneReason string        `json:"done_reason"`
	PromptEval int           `json:"prompt_eval_count"`
	EvalCount  int           `json:"eval_count"`
}

type ollamaTagsResponse struct {
	Models []struct {
		Name string `json:"name"`
	} `json:"models"`
}

func newOllamaBackend(cfg config.OllamaBackendConfig, logger *slog.Logger) backend {
	return &ollamaBackend{
		baseURL: strings.TrimRight(cfg.BaseURL, "/"),
		client:  http.DefaultClient,
		logger:  logger,
	}
}

func (b *ollamaBackend) Name() string {
	return config.LLMBackendOllama
}

func (b *ollamaBackend) Capabilities() Capabilities {
	return Capabilities{
		ToolCalls:    true,
		ImageInput:   true,
		ModelListing: true,
	}
}

func (b *ollamaBackend) Generate(ctx context.Context, req Request) (Response, error) {
	body := ollamaChatRequest{
		Model:  req.Model,
		Stream: false,
	}

	for _, message := range req.Messages {
		body.Messages = append(body.Messages, ollamaMessageFromMessage(message))
	}

	if len(req.Tools) > 0 {
		body.Tools = make([]ollamaToolSchema, 0, len(req.Tools))
		for _, tool := range req.Tools {
			body.Tools = append(body.Tools, ollamaToolSchema{
				Type:     "function",
				Function: ollamaToolDefinition(tool),
			})
		}
	}

	var resp ollamaChatResponse
	if err := b.postJSON(ctx, "/api/chat", body, &resp); err != nil {
		return Response{}, err
	}

	message := messageFromOllama(resp.Message)
	if message.Text() == "" && len(message.ToolCalls) == 0 {
		return Response{}, ErrNoChoices
	}

	return Response{
		Backend:      b.Name(),
		Model:        resp.Model,
		FinishReason: finishReasonFromOllama(resp.DoneReason, message),
		Message:      message,
		Usage: TokenUsage{
			PromptTokens:     resp.PromptEval,
			CompletionTokens: resp.EvalCount,
			TotalTokens:      resp.PromptEval + resp.EvalCount,
		},
	}, nil
}

func (b *ollamaBackend) ListModels(ctx context.Context) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.baseURL+"/api/tags", nil)
	if err != nil {
		return nil, err
	}

	httpResp, err := b.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = httpResp.Body.Close()
	}()

	if httpResp.StatusCode >= http.StatusBadRequest {
		body, _ := io.ReadAll(httpResp.Body)

		return nil, fmt.Errorf("ollama tags request failed: status=%d body=%s", httpResp.StatusCode, string(body))
	}

	var resp ollamaTagsResponse
	if err := json.NewDecoder(httpResp.Body).Decode(&resp); err != nil {
		return nil, err
	}

	models := make([]string, 0, len(resp.Models))
	for _, model := range resp.Models {
		models = append(models, model.Name)
	}

	return models, nil
}

func (b *ollamaBackend) postJSON(ctx context.Context, path string, requestBody any, target any) error {
	body, err := json.Marshal(requestBody)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return err
	}

	req.Header.Set("Content-Type", "application/json")

	resp, err := b.client.Do(req)
	if err != nil {
		return err
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	if resp.StatusCode >= http.StatusBadRequest {
		respBody, _ := io.ReadAll(resp.Body)

		return fmt.Errorf("ollama request failed: status=%d body=%s", resp.StatusCode, string(respBody))
	}

	return json.NewDecoder(resp.Body).Decode(target)
}

func ollamaMessageFromMessage(message Message) ollamaMessage {
	result := ollamaMessage{
		Role:       string(message.Role),
		Name:       message.Name,
		ToolCallID: message.ToolCallID,
	}

	for _, part := range message.Parts {
		switch part.Type {
		case PartTypeText:
			result.Content += part.Text
		case PartTypeImage:
			result.Images = append(result.Images, base64.StdEncoding.EncodeToString(part.ImageData))
		}
	}

	if len(message.ToolCalls) > 0 {
		result.ToolCalls = make([]ollamaToolCall, 0, len(message.ToolCalls))
		for _, call := range message.ToolCalls {
			result.ToolCalls = append(result.ToolCalls, ollamaToolCall{
				Function: ollamaToolCallFunction{
					Name:      call.Name,
					Arguments: call.Arguments,
				},
			})
		}
	}

	return result
}

func messageFromOllama(message ollamaMessage) Message {
	result := Message{
		Role:       Role(message.Role),
		Name:       message.Name,
		ToolCallID: message.ToolCallID,
	}

	if message.Content != "" {
		result.Parts = append(result.Parts, Part{Type: PartTypeText, Text: message.Content})
	}

	if len(message.ToolCalls) > 0 {
		result.ToolCalls = make([]ToolCall, 0, len(message.ToolCalls))
		for i, call := range message.ToolCalls {
			result.ToolCalls = append(result.ToolCalls, ToolCall{
				ID:        fmt.Sprintf("ollama-call-%d", i),
				Name:      call.Function.Name,
				Arguments: call.Function.Arguments,
			})
		}
	}

	return result
}

func finishReasonFromOllama(reason string, message Message) FinishReason {
	if reason == "tool_calls" || len(message.ToolCalls) > 0 {
		return FinishReasonToolCalls
	}

	return FinishReasonStop
}
