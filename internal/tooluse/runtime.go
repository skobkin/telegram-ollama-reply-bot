package tooluse

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"unicode/utf8"

	"telegram-ollama-reply-bot/internal/content/extractor"
	"telegram-ollama-reply-bot/internal/llm"
	"telegram-ollama-reply-bot/internal/logging"
	"telegram-ollama-reply-bot/internal/state"

	t "github.com/mymmrac/telego"
)

var ErrToolLoopLimitReached = errors.New("tool loop iteration limit reached")

const defaultToolResultBudget = 1800

type llmService interface {
	BuildToolUseRequest(ctx context.Context, scope llm.PromptScope, requestContext llm.ChatReplyContext, tools []llm.ToolDefinition, toolPolicy string, extraMessages []llm.Message) (llm.Request, error)
	Generate(ctx context.Context, req llm.Request) (llm.Response, error)
	HandleChatMessage(ctx context.Context, scope llm.PromptScope, requestContext llm.ChatReplyContext) (string, *llm.TokenUsage, error)
}

type PollSender interface {
	SendPoll(ctx context.Context, params *t.SendPollParams) (*t.Message, error)
}

type Config struct {
	MaxIterations int
}

type ChatRequest struct {
	Scope          state.ConversationScope
	PromptScope    llm.PromptScope
	ReplyContext   llm.ChatReplyContext
	RequestMessage state.Message
}

type Runtime struct {
	llm       llmService
	history   state.ConversationStore
	extractor extractor.Extractor
	polls     PollSender
	logger    *slog.Logger
	config    Config
	registry  *Registry
}

func New(llmService llmService, history state.ConversationStore, extractor extractor.Extractor, polls PollSender, logger *slog.Logger, cfg Config) *Runtime {
	if logger == nil {
		logger = slog.Default()
	}
	if cfg.MaxIterations <= 0 {
		cfg.MaxIterations = 6
	}

	runtime := &Runtime{
		llm:       llmService,
		history:   history,
		extractor: extractor,
		polls:     polls,
		logger:    logger,
		config:    cfg,
	}
	runtime.registry = newRegistry(runtime)

	return runtime
}

func (r *Runtime) HandleChatMessage(ctx context.Context, req ChatRequest) (string, *llm.TokenUsage, error) {
	logger := logging.FromContext(ctx, r.logger)
	definitions := r.registry.DefaultDefinitions()
	modelTools := toLLMToolDefinitions(definitions)
	toolPolicy := buildToolPolicy(definitions)

	var (
		extraMessages []llm.Message
		totalUsage    llm.TokenUsage
		started       bool
	)

	for i := 0; i < r.config.MaxIterations; i++ {
		request, err := r.llm.BuildToolUseRequest(ctx, req.PromptScope, req.ReplyContext, modelTools, toolPolicy, extraMessages)
		if err != nil {
			if !started {
				logger.Warn("tool-use request preparation failed, falling back to chat", "error", err)

				return r.llm.HandleChatMessage(ctx, req.PromptScope, req.ReplyContext)
			}

			return "", usagePointer(totalUsage), err
		}

		resp, err := r.llm.Generate(ctx, request)
		if err != nil {
			if !started {
				logger.Warn("tool-use backend failed, falling back to chat", "error", err)

				return r.llm.HandleChatMessage(ctx, req.PromptScope, req.ReplyContext)
			}

			return "", usagePointer(totalUsage), err
		}

		started = true
		accumulateUsage(&totalUsage, resp.Usage)

		if len(resp.Message.ToolCalls) == 0 {
			if resp.Message.Text() == "" {
				return "", usagePointer(totalUsage), llm.ErrNoChoices
			}

			return resp.Message.Text(), usagePointer(totalUsage), nil
		}

		extraMessages = append(extraMessages, resp.Message)

		for _, call := range resp.Message.ToolCalls {
			toolMessage := r.executeToolCall(ctx, req, call)
			extraMessages = append(extraMessages, toolMessage)
		}
	}

	return "", usagePointer(totalUsage), ErrToolLoopLimitReached
}

func (r *Runtime) executeToolCall(ctx context.Context, req ChatRequest, call llm.ToolCall) llm.Message {
	logger := logging.FromContext(ctx, r.logger)
	definition, ok := r.registry.Lookup(call.Name)
	if !ok {
		logger.Warn("tool call references unknown tool", "tool_name", call.Name, "tool_call_id", call.ID)

		return toolResponseMessage(call.ID, marshalResult(toolResult{
			Status: "error",
			Error:  fmt.Sprintf("unknown tool %q", call.Name),
		}, defaultToolResultBudget))
	}

	logger.Info(
		"executing tool call",
		"tool_name", definition.Name,
		"tool_call_id", call.ID,
		"invocation_policy", definition.InvocationPolicy,
		"implementation_status", definition.ImplementationStatus,
		"request_id", logging.RequestIDFromContext(ctx),
	)

	result, err := definition.Handler(ctx, CallContext{
		Scope:     req.Scope,
		RequestID: logging.RequestIDFromContext(ctx),
		Requester: req.RequestMessage,
		Logger:    logger.With("tool_name", definition.Name, "tool_call_id", call.ID),
	}, call.Arguments)
	if err != nil {
		logger.Warn("tool call failed", "tool_name", definition.Name, "tool_call_id", call.ID, "error", err)
		result = toolResult{
			Status: "error",
			Error:  err.Error(),
		}
	}

	return toolResponseMessage(call.ID, marshalResult(result, definition.ResultBudget))
}

func toolResponseMessage(toolCallID, text string) llm.Message {
	return llm.Message{
		Role:       llm.RoleTool,
		ToolCallID: toolCallID,
		Parts: []llm.Part{
			{Type: llm.PartTypeText, Text: text},
		},
	}
}

func toLLMToolDefinitions(definitions []Definition) []llm.ToolDefinition {
	result := make([]llm.ToolDefinition, 0, len(definitions))
	for _, definition := range definitions {
		result = append(result, llm.ToolDefinition{
			Name:        definition.Name,
			Description: definition.Description,
			Parameters:  definition.Parameters,
		})
	}

	return result
}

func buildToolPolicy(definitions []Definition) string {
	discretionary := make([]string, 0, len(definitions))
	explicitOnly := make([]string, 0, len(definitions))
	stubs := make([]string, 0, len(definitions))

	for _, definition := range definitions {
		line := fmt.Sprintf("- %s: %s", definition.Name, definition.Description)
		switch definition.InvocationPolicy {
		case InvocationPolicyDiscretionary:
			discretionary = append(discretionary, line)
		default:
			explicitOnly = append(explicitOnly, line)
		}

		if definition.ImplementationStatus == ImplementationStatusStub {
			stubs = append(stubs, fmt.Sprintf("- %s currently returns a structured not_implemented result.", definition.Name))
		}
	}

	var sections []string
	if len(discretionary) > 0 {
		sections = append(sections, "Discretionary tools: use these whenever they help you answer more accurately.\n"+strings.Join(discretionary, "\n"))
	}
	if len(explicitOnly) > 0 {
		sections = append(sections, "Explicit-request-only tools: use these only when the user clearly asks for that action or lookup.\n"+strings.Join(explicitOnly, "\n"))
	}
	if len(stubs) > 0 {
		sections = append(sections, "Stub tools:\n"+strings.Join(stubs, "\n"))
	}

	return strings.Join(sections, "\n\n")
}

func marshalResult(result toolResult, budget int) string {
	if budget <= 0 {
		budget = defaultToolResultBudget
	}

	data, err := json.Marshal(result)
	if err != nil {
		data = []byte(`{"status":"error","error":"failed to serialize tool result"}`)
	}

	return truncateUTF8(string(data), budget)
}

func truncateUTF8(text string, limit int) string {
	if limit <= 0 || utf8.RuneCountInString(text) <= limit {
		return text
	}

	runes := []rune(text)
	if limit <= 1 {
		return string(runes[:limit])
	}

	return string(runes[:limit-1]) + "…"
}

func accumulateUsage(total *llm.TokenUsage, next llm.TokenUsage) {
	total.PromptTokens += next.PromptTokens
	total.CompletionTokens += next.CompletionTokens
	total.TotalTokens += next.TotalTokens
	total.Cost += next.Cost
}

func usagePointer(usage llm.TokenUsage) *llm.TokenUsage {
	return &usage
}
