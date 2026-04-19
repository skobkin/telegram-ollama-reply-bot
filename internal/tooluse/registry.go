package tooluse

import (
	"context"
	"encoding/json"
	"net/url"
	"slices"
	"strings"

	"telegram-ollama-reply-bot/internal/state"
)

type InvocationPolicy string

const (
	InvocationPolicyExplicitRequestOnly InvocationPolicy = "explicit_request_only"
	InvocationPolicyDiscretionary       InvocationPolicy = "discretionary"
)

type Handler func(ctx context.Context, callCtx CallContext, args json.RawMessage) (toolResult, error)

type Definition struct {
	Name             string
	Description      string
	Parameters       json.RawMessage
	InvocationPolicy InvocationPolicy
	ResultCharBudget int
	SideEffecting    bool
	Handler          Handler
}

type CallContext struct {
	Scope     state.ConversationScope
	RequestID string
	Requester state.Message
	Logger    logger
}

type logger interface {
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
}

type toolResult struct {
	Status  string `json:"status"`
	Summary string `json:"summary,omitempty"`
	Error   string `json:"error,omitempty"`
	Data    any    `json:"data,omitempty"`
}

type Registry struct {
	definitions map[string]Definition
	defaults    []string
}

func newRegistry(runtime *Runtime) *Registry {
	definitions := []Definition{
		{
			Name:             "fetch_url_content",
			Description:      "Fetch and extract article-like content from a URL when the user explicitly asks to inspect, explain, summarize, or analyze a link.",
			Parameters:       json.RawMessage(`{"type":"object","properties":{"url":{"type":"string","description":"HTTP or HTTPS URL to fetch"}},"required":["url"],"additionalProperties":false}`),
			InvocationPolicy: InvocationPolicyExplicitRequestOnly,
			ResultCharBudget: fetchURLContentResultCharBudget,
			Handler:          runtime.fetchURLContent,
		},
		{
			Name:             "search_recent_history",
			Description:      "Search the current chat and current topic recent in-memory history for evidence snippets that help answer recall questions.",
			Parameters:       json.RawMessage(`{"type":"object","properties":{"query":{"type":"string","description":"Optional search query. If empty, return the most recent messages instead."},"limit":{"type":"integer","minimum":1,"maximum":10,"description":"Maximum number of snippets to return."}},"additionalProperties":false}`),
			InvocationPolicy: InvocationPolicyDiscretionary,
			ResultCharBudget: searchRecentHistoryResultCharBudget,
			Handler:          runtime.searchRecentHistory,
		},
		{
			Name:             "get_conversation_summary",
			Description:      "Read the current chat and current topic in-memory earlier summary when it exists.",
			Parameters:       json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
			InvocationPolicy: InvocationPolicyDiscretionary,
			ResultCharBudget: conversationSummaryResultCharBudget,
			Handler:          runtime.getConversationSummary,
		},
		{
			Name:             "create_poll",
			Description:      "Create a regular Telegram poll in the current chat when the user explicitly asks to make a vote or poll.",
			Parameters:       json.RawMessage(`{"type":"object","properties":{"question":{"type":"string","description":"Poll question text"},"options":{"type":"array","items":{"type":"string"},"minItems":2,"maxItems":10,"description":"Poll answer options"},"allows_multiple_answers":{"type":"boolean","description":"Whether voters may choose more than one option"}},"required":["question","options"],"additionalProperties":false}`),
			InvocationPolicy: InvocationPolicyExplicitRequestOnly,
			ResultCharBudget: createPollResultCharBudget,
			SideEffecting:    true,
			Handler:          runtime.createPoll,
		},
		{
			Name:             "list_chat_schedule",
			Description:      "List reminders or scheduled items for the current chat when the user explicitly asks about reminders or schedule. This tool is not implemented yet.",
			Parameters:       json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
			InvocationPolicy: InvocationPolicyExplicitRequestOnly,
			ResultCharBudget: reminderStubResultCharBudget,
			Handler:          reminderStubHandler("list_chat_schedule"),
		},
		{
			Name:             "add_schedule_item",
			Description:      "Create a reminder or scheduled item for the current chat when the user explicitly asks for a reminder. This tool is not implemented yet.",
			Parameters:       json.RawMessage(`{"type":"object","properties":{"request":{"type":"string","description":"Natural-language reminder request from the user"}},"required":["request"],"additionalProperties":false}`),
			InvocationPolicy: InvocationPolicyExplicitRequestOnly,
			ResultCharBudget: reminderStubResultCharBudget,
			Handler:          reminderStubHandler("add_schedule_item"),
		},
		{
			Name:             "remove_schedule_item",
			Description:      "Remove a reminder or scheduled item for the current chat when the user explicitly asks to cancel one. This tool is not implemented yet.",
			Parameters:       json.RawMessage(`{"type":"object","properties":{"target":{"type":"string","description":"Reminder identifier or user-facing description to remove"}},"required":["target"],"additionalProperties":false}`),
			InvocationPolicy: InvocationPolicyExplicitRequestOnly,
			ResultCharBudget: reminderStubResultCharBudget,
			Handler:          reminderStubHandler("remove_schedule_item"),
		},
	}

	result := &Registry{
		definitions: make(map[string]Definition, len(definitions)),
		defaults:    make([]string, 0, len(definitions)),
	}

	for _, definition := range definitions {
		result.definitions[definition.Name] = definition
		result.defaults = append(result.defaults, definition.Name)
	}

	return result
}

func (r *Registry) DefaultDefinitions() []Definition {
	result := make([]Definition, 0, len(r.defaults))
	for _, name := range r.defaults {
		result = append(result, r.definitions[name])
	}

	return result
}

func (r *Registry) Lookup(name string) (Definition, bool) {
	definition, ok := r.definitions[name]

	return definition, ok
}

func isValidToolURL(raw string) bool {
	parsed, err := url.ParseRequestURI(raw)
	if err != nil {
		return false
	}

	return slices.Contains([]string{"http", "https"}, strings.ToLower(parsed.Scheme))
}
