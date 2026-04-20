package llm

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"telegram-ollama-reply-bot/internal/config"
)

func TestOllamaBackendGenerateWithToolsAndImage(t *testing.T) {
	t.Parallel()

	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/api/chat" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}

		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}

		payload := string(body)
		if !strings.Contains(payload, `"model":"gemma3:27b"`) {
			t.Fatalf("request missing model: %s", payload)
		}

		if !strings.Contains(payload, `"tools":[`) {
			t.Fatalf("request missing tools: %s", payload)
		}

		if !strings.Contains(payload, `"images":[`) {
			t.Fatalf("request missing images: %s", payload)
		}

		return jsonHTTPResponse(`{
			"model":"gemma3:27b",
			"message":{
				"role":"assistant",
				"content":"Need a tool",
				"tool_calls":[
					{
						"function":{
							"name":"search_history",
							"arguments":{"query":"reminder"}
						}
					}
				]
			},
			"done_reason":"tool_calls",
			"prompt_eval_count":10,
			"eval_count":4
		}`)
	})}

	impl := newOllamaBackend(config.OllamaBackendConfig{
		BaseURL: "http://ollama.test",
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	impl.(*ollamaBackend).client = client

	resp, err := impl.Generate(context.Background(), Request{
		Feature: FeatureToolUse,
		Model:   "gemma3:27b",
		Messages: []Message{
			{
				Role: RoleUser,
				Parts: []Part{
					{Type: PartTypeText, Text: "Look at this"},
					{Type: PartTypeImage, MIMEType: "image/jpeg", ImageData: []byte("jpeg")},
				},
			},
		},
		Tools: []ToolDefinition{
			{
				Name:        "search_history",
				Description: "Search history",
				Parameters:  json.RawMessage(`{"type":"object"}`),
			},
		},
	})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}

	if resp.FinishReason != FinishReasonToolCalls {
		t.Fatalf("unexpected finish reason: %s", resp.FinishReason)
	}

	if len(resp.Message.ToolCalls) != 1 {
		t.Fatalf("unexpected tool call count: %d", len(resp.Message.ToolCalls))
	}
}

func TestOllamaBackendListModels(t *testing.T) {
	t.Parallel()

	impl := &ollamaBackend{
		baseURL: "http://ollama.test",
		client: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if r.URL.Path != "/api/tags" {
				t.Fatalf("unexpected path: %s", r.URL.Path)
			}

			if r.Method != http.MethodGet {
				t.Fatalf("unexpected method: %s", r.Method)
			}

			return jsonHTTPResponse(`{"models":[{"name":"gemma3:27b"},{"name":"gemma3:12b"}]}`)
		})},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	models, err := impl.ListModels(context.Background())
	if err != nil {
		t.Fatalf("list models: %v", err)
	}

	if len(models) != 2 {
		t.Fatalf("unexpected models count: %d", len(models))
	}
}
