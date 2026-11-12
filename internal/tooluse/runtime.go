package tooluse

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"telegram-ollama-reply-bot/internal/content/extractor"
	"telegram-ollama-reply-bot/internal/content/search"
	"telegram-ollama-reply-bot/internal/execution"
	"telegram-ollama-reply-bot/internal/llm"
	"telegram-ollama-reply-bot/internal/logging"
	"telegram-ollama-reply-bot/internal/reminders"
	"telegram-ollama-reply-bot/internal/state"

	t "github.com/mymmrac/telego"
)

var ErrToolLoopLimitReached = errors.New("tool loop iteration limit reached")

type llmService interface {
	BuildChatToolRequest(ctx context.Context, scope llm.PromptScope, requestContext llm.ChatReplyContext, tools []llm.ToolDefinition, toolPolicy string, extraMessages []llm.Message) (llm.Request, error)
	Generate(ctx context.Context, req llm.Request) (llm.Response, error)
}

type TelegramActionSender interface {
	SendPoll(ctx context.Context, params *t.SendPollParams) (*t.Message, error)
	SendDice(ctx context.Context, params *t.SendDiceParams) (*t.Message, error)
}

// ImageAnalyzer re-analyzes a retained image under a model-authored prompt.
// It is implemented by the bot (which owns the Telegram fetch pipeline and
// the vision wiring); the interface keeps Telegram specifics out of tooluse.
// Results are ephemeral: the analyzer never touches the baseline description
// cache or stored messages.
type ImageAnalyzer interface {
	AnalyzeImage(ctx context.Context, meta state.ImageMeta, prompt string) (string, error)
}

type Config struct {
	MaxIterations      int
	AdminIDs           []int64
	RecentHistoryLimit int
	// Dispatcher receives execution events. Nil disables event emission;
	// delivery is best-effort and never influences the run outcome.
	Dispatcher execution.Dispatcher
}

type ChatRequest struct {
	Scope          state.ConversationScope
	PromptScope    llm.PromptScope
	ReplyContext   llm.ChatReplyContext
	RequestMessage state.Message
	// RunID correlates execution events for this run across subscribers;
	// generated when empty.
	RunID string
}

// ToolCallRecord is the authoritative record of one tool invocation, in
// execution order (including failed and unknown-tool calls). It feeds
// presentation such as the final tool-history block; events are best-effort
// and never feed it.
type ToolCallRecord struct {
	CallID     string
	Name       string
	Visibility Visibility
	Status     string
	Duration   time.Duration
}

// Result is the authoritative outcome of a tool-enabled reply: the final
// answer text (possibly empty for side-effect-only completions), the
// accumulated token usage, and the chronological tool-call history.
type Result struct {
	Reply     string
	Usage     *llm.TokenUsage
	ToolCalls []ToolCallRecord
}

// runEmitter publishes execution events for one run. Emission is guarded by
// the dispatcher being configured at all; publishing never blocks or fails.
type runEmitter struct {
	dispatcher execution.Dispatcher
	rc         execution.RunContext
}

func (e runEmitter) publish(event execution.Event) {
	if e.dispatcher == nil {
		return
	}
	e.dispatcher.Publish(event)
}

// classifyRunFailure maps an error to a stable RunFailed reason classifier.
// Raw error strings never appear on events.
func classifyRunFailure(err error, fallback string) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return execution.ReasonTimeout
	case errors.Is(err, context.Canceled):
		return execution.ReasonCancelled
	case errors.Is(err, ErrToolLoopLimitReached):
		return execution.ReasonLoopLimit
	case errors.Is(err, llm.ErrNoChoices):
		return execution.ReasonNoChoices
	default:
		return fallback
	}
}

type Runtime struct {
	llm           llmService
	history       state.ConversationStore
	extractor     extractor.Extractor
	searcher      search.Searcher
	actions       TelegramActionSender
	imageAnalyzer ImageAnalyzer
	reminders     *reminders.Service
	logger        *slog.Logger
	config        Config
	registry      *Registry
	closers       []registeredCloser
}

type registeredCloser struct {
	name   string
	closer func() error
}

func New(llmService llmService, history state.ConversationStore, extractor extractor.Extractor, searcher search.Searcher, actions TelegramActionSender, reminderService *reminders.Service, logger *slog.Logger, cfg Config) *Runtime {
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
		actions:   actions,
		reminders: reminderService,
		logger:    logger,
		config:    cfg,
	}
	runtime.registry = newRegistry(runtime)

	return runtime
}

// MCPToolSource is the minimal contract the runtime needs from an MCP
// manager: a slice of translated definitions to install, and a closer to
// call at shutdown. The interface keeps `tooluse` from importing the MCP
// package directly and lets tests stub the manager trivially.
type MCPToolSource interface {
	Definitions() []Definition
	Close() error
}

// AttachImageAnalyzer wires the bot's image analyzer and registers the image
// discovery/analysis tools. It is a no-op when `a` is nil so the application
// layer can gate it on the image-recognition enable flag; while detached the
// tools are absent from the registry and never advertised to the model. Like
// AttachMCP, it runs after runtime construction because the bot is built
// with the runtime as a dependency. Re-calling replaces the prior analyzer
// and definitions.
func (r *Runtime) AttachImageAnalyzer(a ImageAnalyzer) {
	if a == nil {
		return
	}
	r.imageAnalyzer = a
	r.registry.Register(Definition{
		Name:             "list_recent_images",
		Description:      "List recent image-bearing messages in the current chat/topic so you can identify the right image by message_id before a detailed analysis.",
		Parameters:       json.RawMessage(`{"type":"object","properties":{"limit":{"type":"integer","minimum":1,"maximum":10,"description":"Maximum number of images to return."}},"additionalProperties":false}`),
		InvocationPolicy: InvocationPolicyDiscretionary,
		ResultCharBudget: listImagesResultCharBudget,
		Visibility:       VisibilityStandard,
		Handler:          r.listRecentImages,
	})
	r.registry.Register(Definition{
		Name:             "analyze_image",
		Description:      "Re-analyze an image from a recent message under your own focused prompt when the cached baseline description is insufficient. Identify the message_id via list_recent_images or get_message_thread_context first. Each call is a paid recognition request; avoid casual or repeated use.",
		Parameters:       json.RawMessage(`{"type":"object","properties":{"message_id":{"type":"integer","minimum":1,"description":"Telegram message_id of the image-bearing message, from tool results."},"prompt":{"type":"string","minLength":1,"maxLength":4000,"description":"Focused analysis prompt for this image, e.g. 'read the error text in the screenshot'."}},"required":["message_id","prompt"],"additionalProperties":false}`),
		InvocationPolicy: InvocationPolicyDiscretionaryPaid,
		ResultCharBudget: analyzeImageResultCharBudget,
		Visibility:       VisibilityStandard,
		Handler:          r.analyzeImage,
	})
}

// AttachMCP registers every tool the source currently advertises and wires
// a shutdown closer. It is a no-op when `source` is nil so the application
// layer can call it unconditionally. Re-calling replaces the previous source.
func (r *Runtime) AttachMCP(name string, source MCPToolSource) {
	if source == nil {
		return
	}
	for _, def := range source.Definitions() {
		r.registry.Register(def)
	}
	r.RegisterCloser(name, source.Close)
}

// HasTool reports whether a tool with the given name is currently registered
// (and therefore advertised to the model).
func (r *Runtime) HasTool(name string) bool {
	_, ok := r.registry.Lookup(name)

	return ok
}

// RegisterCloser adds a shutdown hook (e.g. closing an MCP session pool).
// Closers run LIFO on Close; duplicate names replace the previous closer so
// the same source can be re-attached without leaking stale shutdown hooks.
func (r *Runtime) RegisterCloser(name string, closer func() error) {
	if closer == nil {
		return
	}
	for i, existing := range r.closers {
		if existing.name == name {
			r.closers[i].closer = closer

			return
		}
	}
	r.closers = append(r.closers, registeredCloser{name: name, closer: closer})
}

// Close runs every registered closer in LIFO order, logging (but not
// propagating) individual errors so the caller can use it from a defer.
func (r *Runtime) Close() {
	if len(r.closers) == 0 {
		return
	}
	for i := len(r.closers) - 1; i >= 0; i-- {
		if err := r.closers[i].closer(); err != nil {
			r.logger.Warn("tool runtime closer failed", "closer", r.closers[i].name, "error", err)
		}
	}
	r.closers = nil
}

// ReplyWithTools runs the tool-enabled generation loop and returns the
// authoritative Result. Execution events are published along the way, but
// delivery is best-effort: the returned outcome never depends on it.
func (r *Runtime) ReplyWithTools(ctx context.Context, req ChatRequest) (Result, error) {
	logger := logging.FromContext(ctx, r.logger)
	definitions := r.registry.DefaultDefinitions()
	modelTools := toLLMToolDefinitions(definitions)
	toolPolicy := buildToolPolicy(definitions)

	runID := req.RunID
	if runID == "" {
		runID = execution.NewRunID()
	}
	emitter := runEmitter{dispatcher: r.config.Dispatcher}
	emitter.rc = execution.RunContext{
		RunID:     runID,
		ChatID:    req.Scope.ChatID,
		TopicID:   req.Scope.TopicID,
		MessageID: req.RequestMessage.MessageID,
	}
	runStart := time.Now()
	emitter.publish(execution.RunStarted{RunContext: emitter.rc, At: runStart})

	var (
		extraMessages             []llm.Message
		totalUsage                llm.TokenUsage
		records                   []ToolCallRecord
		started                   bool
		allowEmptyReplyCompletion bool
	)

	succeeded := func(reply string) (Result, error) {
		emitter.publish(execution.RunCompleted{
			RunContext:  emitter.rc,
			At:          time.Now(),
			Duration:    time.Since(runStart),
			ReplyLength: len([]rune(reply)),
		})

		return Result{Reply: reply, Usage: usagePointer(totalUsage), ToolCalls: records}, nil
	}
	fail := func(err error, fallbackReason string) (Result, error) {
		emitter.publish(execution.RunFailed{
			RunContext: emitter.rc,
			At:         time.Now(),
			Duration:   time.Since(runStart),
			Reason:     classifyRunFailure(err, fallbackReason),
		})

		return Result{Usage: usagePointer(totalUsage), ToolCalls: records}, err
	}

	for i := 0; i < r.config.MaxIterations; i++ {
		genStart := time.Now()
		emitter.publish(execution.GenerationStarted{RunContext: emitter.rc, At: genStart, Iteration: i})

		request, err := r.llm.BuildChatToolRequest(ctx, req.PromptScope, req.ReplyContext, modelTools, toolPolicy, extraMessages)
		if err != nil {
			if !started {
				logger.Warn("chat request preparation failed", "error", err)
			}

			return fail(err, execution.ReasonPrepare)
		}

		resp, err := r.llm.Generate(ctx, request)
		if err != nil {
			if started && allowEmptyReplyCompletion && errors.Is(err, llm.ErrNoChoices) {
				return succeeded("")
			}

			if !started {
				logger.Warn("chat backend failed", "error", err)
			}

			return fail(err, execution.ReasonBackend)
		}

		emitter.publish(execution.GenerationCompleted{
			RunContext:    emitter.rc,
			At:            time.Now(),
			Iteration:     i,
			Duration:      time.Since(genStart),
			FinishReason:  string(resp.FinishReason),
			ToolCallCount: len(resp.Message.ToolCalls),
		})

		started = true
		accumulateUsage(&totalUsage, resp.Usage)

		if len(resp.Message.ToolCalls) == 0 {
			text := resp.Message.Text()
			if text == "" {
				if allowEmptyReplyCompletion {
					return succeeded("")
				}

				return fail(llm.ErrNoChoices, execution.ReasonBackend)
			}

			return succeeded(text)
		}

		// Preserve the assistant tool-call message and the corresponding tool outputs.
		// Backends such as OpenAI-style APIs use that pairing to associate tool results
		// with specific tool call IDs on subsequent turns.
		extraMessages = append(extraMessages, resp.Message)
		allowEmptyReplyCompletion = false

		for _, call := range resp.Message.ToolCalls {
			toolMessage, allowsEmptyReply, record := r.executeToolCall(ctx, emitter, req, call)
			records = append(records, record)
			extraMessages = append(extraMessages, toolMessage)
			allowEmptyReplyCompletion = allowEmptyReplyCompletion || allowsEmptyReply
		}
	}

	return fail(ErrToolLoopLimitReached, execution.ReasonBackend)
}

func (r *Runtime) executeToolCall(ctx context.Context, emitter runEmitter, req ChatRequest, call llm.ToolCall) (llm.Message, bool, ToolCallRecord) {
	logger := logging.FromContext(ctx, r.logger)
	definition, ok := r.registry.Lookup(call.Name)
	if !ok {
		logger.Warn("tool call references unknown tool", "tool_name", call.Name, "tool_call_id", call.ID)
		emitter.publish(execution.ToolStarted{RunContext: emitter.rc, At: time.Now(), CallID: call.ID, ToolName: call.Name})
		emitter.publish(execution.ToolCompleted{
			RunContext: emitter.rc,
			At:         time.Now(),
			CallID:     call.ID,
			ToolName:   call.Name,
			Duration:   time.Duration(0),
			Status:     execution.StatusError,
		})

		return toolResponseMessage(call.ID, marshalResult(toolResult{
			Status: "error",
			Error:  fmt.Sprintf("unknown tool %q", call.Name),
		}, defaultToolResultCharBudget)), false, ToolCallRecord{
			CallID:     call.ID,
			Name:       call.Name,
			Visibility: VisibilityStandard,
			Status:     execution.StatusError,
		}
	}

	logger.Info(
		"executing tool call",
		"tool_name", definition.Name,
		"tool_call_id", call.ID,
		"invocation_policy", definition.InvocationPolicy,
	)

	internal := definition.EffectiveVisibility() == VisibilityInternal
	toolStart := time.Now()
	emitter.publish(execution.ToolStarted{
		RunContext: emitter.rc,
		At:         toolStart,
		CallID:     call.ID,
		ToolName:   definition.Name,
		Internal:   internal,
	})

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

	duration := time.Since(toolStart)
	emitter.publish(execution.ToolCompleted{
		RunContext: emitter.rc,
		At:         time.Now(),
		CallID:     call.ID,
		ToolName:   definition.Name,
		Internal:   internal,
		Duration:   duration,
		Status:     result.Status,
	})

	return toolResponseMessage(call.ID, marshalResult(result, definition.ResultCharBudget)),
		definition.SideEffecting && result.Status == execution.StatusOK,
		ToolCallRecord{
			CallID:     call.ID,
			Name:       definition.Name,
			Visibility: definition.EffectiveVisibility(),
			Status:     result.Status,
			Duration:   duration,
		}
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
