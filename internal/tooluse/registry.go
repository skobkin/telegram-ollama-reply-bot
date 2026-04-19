package tooluse

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	"telegram-ollama-reply-bot/internal/llmcontext"
	"telegram-ollama-reply-bot/internal/state"

	t "github.com/mymmrac/telego"
	tu "github.com/mymmrac/telego/telegoutil"
)

type InvocationPolicy string

const (
	InvocationPolicyExplicitRequestOnly InvocationPolicy = "explicit_request_only"
	InvocationPolicyDiscretionary       InvocationPolicy = "discretionary"
)

type ImplementationStatus string

const (
	ImplementationStatusReady ImplementationStatus = "ready"
	ImplementationStatusStub  ImplementationStatus = "stub"
)

type Handler func(ctx context.Context, callCtx CallContext, args json.RawMessage) (toolResult, error)

type Definition struct {
	Name                 string
	Description          string
	Parameters           json.RawMessage
	InvocationPolicy     InvocationPolicy
	ImplementationStatus ImplementationStatus
	ResultBudget         int
	SideEffecting        bool
	Handler              Handler
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
			Name:                 "fetch_url_content",
			Description:          "Fetch and extract article-like content from a URL when the user explicitly asks to inspect, explain, summarize, or analyze a link.",
			Parameters:           json.RawMessage(`{"type":"object","properties":{"url":{"type":"string","description":"HTTP or HTTPS URL to fetch"}},"required":["url"],"additionalProperties":false}`),
			InvocationPolicy:     InvocationPolicyExplicitRequestOnly,
			ImplementationStatus: ImplementationStatusReady,
			ResultBudget:         3500,
			Handler:              runtime.fetchURLContent,
		},
		{
			Name:                 "search_recent_history",
			Description:          "Search the current chat and current topic recent in-memory history for evidence snippets that help answer recall questions.",
			Parameters:           json.RawMessage(`{"type":"object","properties":{"query":{"type":"string","description":"Optional search query. If empty, return the most recent messages instead."},"limit":{"type":"integer","minimum":1,"maximum":10,"description":"Maximum number of snippets to return."}},"additionalProperties":false}`),
			InvocationPolicy:     InvocationPolicyDiscretionary,
			ImplementationStatus: ImplementationStatusReady,
			ResultBudget:         2200,
			Handler:              runtime.searchRecentHistory,
		},
		{
			Name:                 "get_conversation_summary",
			Description:          "Read the current chat and current topic in-memory earlier summary when it exists.",
			Parameters:           json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
			InvocationPolicy:     InvocationPolicyDiscretionary,
			ImplementationStatus: ImplementationStatusReady,
			ResultBudget:         1800,
			Handler:              runtime.getConversationSummary,
		},
		{
			Name:                 "create_poll",
			Description:          "Create a regular Telegram poll in the current chat when the user explicitly asks to make a vote or poll.",
			Parameters:           json.RawMessage(`{"type":"object","properties":{"question":{"type":"string","description":"Poll question text"},"options":{"type":"array","items":{"type":"string"},"minItems":2,"maxItems":10,"description":"Poll answer options"},"allows_multiple_answers":{"type":"boolean","description":"Whether voters may choose more than one option"}},"required":["question","options"],"additionalProperties":false}`),
			InvocationPolicy:     InvocationPolicyExplicitRequestOnly,
			ImplementationStatus: ImplementationStatusReady,
			ResultBudget:         1200,
			SideEffecting:        true,
			Handler:              runtime.createPoll,
		},
		{
			Name:                 "list_chat_schedule",
			Description:          "List reminders or scheduled items for the current chat when the user explicitly asks about reminders or schedule.",
			Parameters:           json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
			InvocationPolicy:     InvocationPolicyExplicitRequestOnly,
			ImplementationStatus: ImplementationStatusStub,
			ResultBudget:         900,
			Handler:              reminderStubHandler("list_chat_schedule"),
		},
		{
			Name:                 "add_schedule_item",
			Description:          "Create a reminder or scheduled item for the current chat when the user explicitly asks for a reminder.",
			Parameters:           json.RawMessage(`{"type":"object","properties":{"request":{"type":"string","description":"Natural-language reminder request from the user"}},"required":["request"],"additionalProperties":false}`),
			InvocationPolicy:     InvocationPolicyExplicitRequestOnly,
			ImplementationStatus: ImplementationStatusStub,
			ResultBudget:         900,
			Handler:              reminderStubHandler("add_schedule_item"),
		},
		{
			Name:                 "remove_schedule_item",
			Description:          "Remove a reminder or scheduled item for the current chat when the user explicitly asks to cancel one.",
			Parameters:           json.RawMessage(`{"type":"object","properties":{"target":{"type":"string","description":"Reminder identifier or user-facing description to remove"}},"required":["target"],"additionalProperties":false}`),
			InvocationPolicy:     InvocationPolicyExplicitRequestOnly,
			ImplementationStatus: ImplementationStatusStub,
			ResultBudget:         900,
			Handler:              reminderStubHandler("remove_schedule_item"),
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

func (r *Runtime) fetchURLContent(ctx context.Context, callCtx CallContext, args json.RawMessage) (toolResult, error) {
	var payload struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(args, &payload); err != nil {
		return toolResult{}, fmt.Errorf("parse arguments: %w", err)
	}
	if !isValidToolURL(payload.URL) {
		return toolResult{}, fmt.Errorf("url must use http or https")
	}

	article, err := r.extractor.GetArticleFromURL(ctx, payload.URL)
	if err != nil {
		return toolResult{}, err
	}

	return toolResult{
		Status:  "ok",
		Summary: "URL content fetched",
		Data: map[string]any{
			"chat_id":     callCtx.Scope.ChatID,
			"topic_id":    callCtx.Scope.TopicID,
			"url":         article.URL,
			"title":       article.Title,
			"text":        truncateUTF8(strings.TrimSpace(article.Text), 2600),
			"extractedAt": time.Now().UTC().Format(time.RFC3339),
		},
	}, nil
}

func (r *Runtime) searchRecentHistory(_ context.Context, callCtx CallContext, args json.RawMessage) (toolResult, error) {
	var payload struct {
		Query string `json:"query"`
		Limit int    `json:"limit"`
	}
	if err := json.Unmarshal(args, &payload); err != nil && string(args) != "" && string(args) != "null" {
		return toolResult{}, fmt.Errorf("parse arguments: %w", err)
	}
	if payload.Limit <= 0 {
		payload.Limit = 5
	}
	if payload.Limit > 10 {
		payload.Limit = 10
	}

	snapshot := r.history.Snapshot(callCtx.Scope)
	matches := make([]map[string]any, 0, payload.Limit)
	query := strings.TrimSpace(strings.ToLower(payload.Query))

	for i := len(snapshot.Messages) - 1; i >= 0 && len(matches) < payload.Limit; i-- {
		msg := snapshot.Messages[i]
		candidate := strings.ToLower(llmcontext.RenderMessagesPlainText([]state.Message{msg}))
		if query != "" && !strings.Contains(candidate, query) {
			continue
		}

		matches = append(matches, map[string]any{
			"name":       msg.Name,
			"username":   msg.Username,
			"text":       truncateUTF8(strings.TrimSpace(msg.Text), 280),
			"message_id": msg.MessageID,
			"from_id":    msg.FromID,
			"created_at": msg.CreatedAt.UTC().Format(time.RFC3339),
		})
	}

	return toolResult{
		Status:  "ok",
		Summary: fmt.Sprintf("Found %d recent history snippets", len(matches)),
		Data: map[string]any{
			"query":    payload.Query,
			"scope":    map[string]any{"chat_id": callCtx.Scope.ChatID, "topic_id": callCtx.Scope.TopicID},
			"matches":  matches,
			"searched": len(snapshot.Messages),
		},
	}, nil
}

func (r *Runtime) getConversationSummary(_ context.Context, callCtx CallContext, _ json.RawMessage) (toolResult, error) {
	snapshot := r.history.Snapshot(callCtx.Scope)
	if strings.TrimSpace(snapshot.EarlierSummary) == "" {
		return toolResult{
			Status:  "empty",
			Summary: "No in-memory conversation summary is available for the current chat/topic",
			Data: map[string]any{
				"chat_id":  callCtx.Scope.ChatID,
				"topic_id": callCtx.Scope.TopicID,
			},
		}, nil
	}

	return toolResult{
		Status:  "ok",
		Summary: "Conversation summary retrieved",
		Data: map[string]any{
			"chat_id":  callCtx.Scope.ChatID,
			"topic_id": callCtx.Scope.TopicID,
			"summary":  truncateUTF8(snapshot.EarlierSummary, 1400),
		},
	}, nil
}

func (r *Runtime) createPoll(ctx context.Context, callCtx CallContext, args json.RawMessage) (toolResult, error) {
	var payload struct {
		Question              string   `json:"question"`
		Options               []string `json:"options"`
		AllowsMultipleAnswers bool     `json:"allows_multiple_answers"`
	}
	if err := json.Unmarshal(args, &payload); err != nil {
		return toolResult{}, fmt.Errorf("parse arguments: %w", err)
	}
	payload.Question = strings.TrimSpace(payload.Question)
	if payload.Question == "" {
		return toolResult{}, errors.New("question is required")
	}
	if len(payload.Options) < 2 || len(payload.Options) > 10 {
		return toolResult{}, errors.New("options must contain 2 to 10 entries")
	}

	options := make([]t.InputPollOption, 0, len(payload.Options))
	for _, option := range payload.Options {
		option = strings.TrimSpace(option)
		if option == "" {
			return toolResult{}, errors.New("poll options must be non-empty")
		}
		options = append(options, t.InputPollOption{Text: option})
	}

	params := &t.SendPollParams{
		ChatID:                tu.ID(callCtx.Scope.ChatID),
		Question:              payload.Question,
		Options:               options,
		Type:                  "regular",
		AllowsMultipleAnswers: payload.AllowsMultipleAnswers,
	}
	if callCtx.Scope.TopicID != 0 {
		params.MessageThreadID = callCtx.Scope.TopicID
	}

	message, err := r.polls.SendPoll(ctx, params)
	if err != nil {
		return toolResult{}, err
	}

	return toolResult{
		Status:  "ok",
		Summary: "Poll created",
		Data: map[string]any{
			"chat_id":    callCtx.Scope.ChatID,
			"topic_id":   callCtx.Scope.TopicID,
			"message_id": message.MessageID,
			"question":   payload.Question,
			"options":    payload.Options,
		},
	}, nil
}

func reminderStubHandler(toolName string) Handler {
	return func(_ context.Context, callCtx CallContext, _ json.RawMessage) (toolResult, error) {
		return toolResult{
			Status:  "not_implemented",
			Summary: fmt.Sprintf("%s is registered but reminder scheduling is not implemented yet", toolName),
			Data: map[string]any{
				"chat_id":  callCtx.Scope.ChatID,
				"topic_id": callCtx.Scope.TopicID,
				"note":     "Future reminder scheduling will persist the originating chat and topic so reminder messages go back to the same topic when one exists.",
			},
		}, nil
	}
}

func isValidToolURL(raw string) bool {
	parsed, err := url.ParseRequestURI(raw)
	if err != nil {
		return false
	}

	return slices.Contains([]string{"http", "https"}, strings.ToLower(parsed.Scheme))
}
