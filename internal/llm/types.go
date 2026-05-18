package llm

import (
	"context"
	"encoding/json"
	"errors"
)

var (
	ErrLlmBackendRequestFailed = errors.New("llm back-end request failed")
	ErrNoChoices               = errors.New("no choices in LLM response")
	ErrTemplateProcessing      = errors.New("template processing failed")
	ErrUnknownBackend          = errors.New("unknown llm backend")
	ErrUnsupportedCapability   = errors.New("llm backend does not support requested capability")
	ErrBackendNotConfigured    = errors.New("llm backend is not configured")
	ErrFeatureRouteInvalid     = errors.New("llm feature route is invalid")
)

type Feature string

const (
	FeatureChat             Feature = "chat"
	FeatureSummarize        Feature = "summarize"
	FeatureImageRecognition Feature = "image_recognition"
)

type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

type PartType string

const (
	PartTypeText  PartType = "text"
	PartTypeImage PartType = "image"
)

type FinishReason string

const (
	FinishReasonStop      FinishReason = "stop"
	FinishReasonToolCalls FinishReason = "tool_calls"
)

type Part struct {
	Type      PartType
	Text      string
	MIMEType  string
	ImageData []byte
}

type Message struct {
	Role       Role
	Name       string
	ToolCallID string
	Parts      []Part
	ToolCalls  []ToolCall
}

func TextMessage(role Role, text string) Message {
	return Message{
		Role: role,
		Parts: []Part{
			{Type: PartTypeText, Text: text},
		},
	}
}

func (m Message) Text() string {
	result := ""

	for _, part := range m.Parts {
		if part.Type == PartTypeText {
			result += part.Text
		}
	}

	return result
}

type ToolDefinition struct {
	Name        string
	Description string
	Parameters  json.RawMessage
}

type ToolCall struct {
	ID        string
	Name      string
	Arguments json.RawMessage
}

type Request struct {
	Feature  Feature
	Model    string
	Messages []Message
	Tools    []ToolDefinition
}

type Response struct {
	Backend      string
	Model        string
	FinishReason FinishReason
	Message      Message
	Usage        TokenUsage
}

type TokenUsage struct {
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	Cost             float64
}

type Capabilities struct {
	ToolCalls    bool
	ImageInput   bool
	ModelListing bool
}

type backend interface {
	Name() string
	Capabilities() Capabilities
	Generate(ctx context.Context, req Request) (Response, error)
	ListModels(ctx context.Context) ([]string, error)
}
