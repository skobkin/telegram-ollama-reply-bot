package tooluse

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"telegram-ollama-reply-bot/internal/content/extractor"
	"telegram-ollama-reply-bot/internal/content/search"
	"telegram-ollama-reply-bot/internal/llm"
	"telegram-ollama-reply-bot/internal/logging"
	"telegram-ollama-reply-bot/internal/reminders"
	"telegram-ollama-reply-bot/internal/state"

	t "github.com/mymmrac/telego"
)

var ErrToolLoopLimitReached = errors.New("tool loop iteration limit reached")
var ErrToolUseUnavailable = errors.New("tool use is unavailable")

type llmService interface {
	BuildToolUseRequest(ctx context.Context, scope llm.PromptScope, requestContext llm.ChatReplyContext, tools []llm.ToolDefinition, toolPolicy string, extraMessages []llm.Message) (llm.Request, error)
	Generate(ctx context.Context, req llm.Request) (llm.Response, error)
}

type PollSender interface {
	SendPoll(ctx context.Context, params *t.SendPollParams) (*t.Message, error)
}

type Config struct {
	MaxIterations      int
	AdminIDs           []int64
	RecentHistoryLimit int
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
	searcher  search.Searcher
	polls     PollSender
	reminders *reminders.Service
	logger    *slog.Logger
	config    Config
	registry  *Registry
}

func New(llmService llmService, history state.ConversationStore, extractor extractor.Extractor, searcher search.Searcher, polls PollSender, reminderService *reminders.Service, logger *slog.Logger, cfg Config) *Runtime {
	if logger == nil {
		logger = slog.Default()
	}
	if cfg.MaxIterations <= 0 {
		cfg.MaxIterations = 6
	}
	if cfg.RecentHistoryLimit < 0 {
		cfg.RecentHistoryLimit = 15
	}

	runtime := &Runtime{
		llm:       llmService,
		history:   history,
		extractor: extractor,
		searcher:  searcher,
		polls:     polls,
		reminders: reminderService,
		logger:    logger,
		config:    cfg,
	}
	runtime.registry = newRegistry(runtime)

	return runtime
}

func (r *Runtime) ReplyWithTools(ctx context.Context, req ChatRequest) (string, *llm.TokenUsage, error) {
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
				logger.Warn("tool-use request preparation failed", "error", err)

				return "", nil, errors.Join(ErrToolUseUnavailable, err)
			}

			return "", usagePointer(totalUsage), err
		}

		resp, err := r.llm.Generate(ctx, request)
		if err != nil {
			if !started {
				logger.Warn("tool-use backend failed", "error", err)

				return "", nil, errors.Join(ErrToolUseUnavailable, err)
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

		// Preserve the assistant tool-call message and the corresponding tool outputs.
		// Backends such as OpenAI-style APIs use that pairing to associate tool results
		// with specific tool call IDs on subsequent turns.
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
		}, defaultToolResultCharBudget))
	}

	logger.Info(
		"executing tool call",
		"tool_name", definition.Name,
		"tool_call_id", call.ID,
		"invocation_policy", definition.InvocationPolicy,
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

	return toolResponseMessage(call.ID, marshalResult(result, definition.ResultCharBudget))
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
	discretionaryPaid := make([]string, 0, len(definitions))
	explicitOnly := make([]string, 0, len(definitions))

	for _, definition := range definitions {
		line := fmt.Sprintf("- %s: %s", definition.Name, definition.Description)
		switch definition.InvocationPolicy {
		case InvocationPolicyDiscretionary:
			discretionary = append(discretionary, line)
		case InvocationPolicyDiscretionaryPaid:
			discretionaryPaid = append(discretionaryPaid, line)
		default:
			explicitOnly = append(explicitOnly, line)
		}
	}

	var sections []string
	if len(discretionary) > 0 {
		sections = append(sections, "Discretionary tools: use these whenever they help you answer more accurately.\n"+strings.Join(discretionary, "\n"))
	}
	if len(discretionaryPaid) > 0 {
		sections = append(sections, "Discretionary paid tools: use these when they are needed for accuracy or freshness, but avoid casual, speculative, or repeated use because they consume paid external resources.\n"+strings.Join(discretionaryPaid, "\n"))
	}
	if len(explicitOnly) > 0 {
		sections = append(sections, "Explicit-request-only tools: use these only when the user clearly asks for that action or lookup.\n"+strings.Join(explicitOnly, "\n"))
	}

	return strings.Join(sections, "\n\n")
}
