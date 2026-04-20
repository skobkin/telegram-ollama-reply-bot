package llm

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/sashabaranov/go-openai"
)

func TestOpenAICompatBackendGenerateWithToolsAndImage(t *testing.T) {
	t.Parallel()

	clientCfg := openai.DefaultConfig("")
	clientCfg.BaseURL = "http://openai-compat.test"
	clientCfg.HTTPClient = roundTripDoer(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/chat/completions" {
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

		if !strings.Contains(payload, `"tool_calls":[`) {
			t.Fatalf("request missing tool call messages: %s", payload)
		}

		if !strings.Contains(payload, `"image_url"`) {
			t.Fatalf("request missing image input: %s", payload)
		}

		return jsonHTTPResponse(`{
			"id":"chatcmpl-1",
			"object":"chat.completion",
			"created":1,
			"model":"gemma3:27b",
			"choices":[
				{
					"index":0,
					"finish_reason":"tool_calls",
					"message":{
						"role":"assistant",
						"content":"Need to check history",
						"tool_calls":[
							{
								"id":"call_1",
								"type":"function",
								"function":{
									"name":"search_history",
									"arguments":"{\"query\":\"reminder\"}"
								}
							}
						]
					}
				}
			],
			"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}
		}`)
	})

	impl := &openAICompatBackend{
		client: openai.NewClientWithConfig(clientCfg),
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	resp, err := impl.Generate(context.Background(), Request{
		Feature: FeatureToolUse,
		Model:   "gemma3:27b",
		Messages: []Message{
			TextMessage(RoleSystem, "system"),
			{
				Role: RoleUser,
				Parts: []Part{
					{Type: PartTypeText, Text: "What is in the screenshot?"},
					{Type: PartTypeImage, MIMEType: "image/jpeg", ImageData: []byte("jpeg")},
				},
			},
			{
				Role: RoleAssistant,
				ToolCalls: []ToolCall{
					{
						ID:        "call_0",
						Name:      "lookup",
						Arguments: json.RawMessage(`{"id":1}`),
					},
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

	if resp.Message.ToolCalls[0].Name != "search_history" {
		t.Fatalf("unexpected tool call name: %s", resp.Message.ToolCalls[0].Name)
	}
}
