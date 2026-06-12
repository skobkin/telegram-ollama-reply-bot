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
	InvocationPolicyDiscretionaryPaid   InvocationPolicy = "discretionary_paid"
)

const (
	defaultToolResultCharBudget = 1800

	fetchURLContentResultCharBudget     = 3500
	searchHistoryResultCharBudget       = 6000
	conversationSummaryResultCharBudget = 1800
	chatActivityResultCharBudget        = 1200
	historyBoundsResultCharBudget       = 900
	messageThreadResultCharBudget       = 2600
	sendPollResultCharBudget            = 1200
	sendQuizResultCharBudget            = 1200
	sendDiceResultCharBudget            = 800
	reminderResultCharBudget            = 1400
	currentTimeResultCharBudget         = 600
	recentLinksResultCharBudget         = 1600
	datetimeMathResultCharBudget        = 1200
	datetimeFormatResultCharBudget      = 700
)

// ToolResult is the value a Handler returns to the runtime. It is exported
// as a type alias so packages outside `tooluse` (notably the MCP adapter in
// `internal/mcp`) can write handler closures that satisfy the Handler type.
type ToolResult = toolResult

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
			Name:             "get_current_time",
			Description:      "Read the current server time and timezone context whenever time-sensitive reasoning would be more accurate with an explicit current timestamp.",
			Parameters:       json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
			InvocationPolicy: InvocationPolicyDiscretionary,
			ResultCharBudget: currentTimeResultCharBudget,
			Handler:          currentTimeHandler,
		},
		{
			Name:             "list_recent_links",
			Description:      "List recent HTTP or HTTPS links mentioned in the current chat/topic history so you can identify the right URL before fetching content.",
			Parameters:       json.RawMessage(`{"type":"object","properties":{"limit":{"type":"integer","minimum":1,"maximum":10,"description":"Maximum number of unique links to return."}},"additionalProperties":false}`),
			InvocationPolicy: InvocationPolicyDiscretionary,
			ResultCharBudget: recentLinksResultCharBudget,
			Handler:          runtime.listRecentLinks,
		},
		{
			Name:             "datetime_math",
			Description:      "Perform exact date/time calculations on RFC3339 timestamps, including diff, shift, weekday lookup, and timezone conversion.",
			Parameters:       json.RawMessage(`{"type":"object","properties":{"operation":{"type":"string","enum":["diff","shift","weekday","convert_timezone"],"description":"Datetime operation to perform."},"timestamp":{"type":"string","description":"RFC3339 timestamp used by shift, weekday, and convert_timezone."},"left":{"type":"string","description":"Left RFC3339 timestamp used by diff."},"right":{"type":"string","description":"Right RFC3339 timestamp used by diff."},"target_timezone":{"type":"string","description":"Target IANA timezone name used by convert_timezone."},"years":{"type":"integer","description":"Signed calendar year delta used by shift."},"months":{"type":"integer","description":"Signed calendar month delta used by shift."},"days":{"type":"integer","description":"Signed calendar day delta used by shift."},"hours":{"type":"integer","description":"Signed hour delta used by shift."},"minutes":{"type":"integer","description":"Signed minute delta used by shift."},"seconds":{"type":"integer","description":"Signed second delta used by shift."}},"required":["operation"],"additionalProperties":false}`),
			InvocationPolicy: InvocationPolicyDiscretionary,
			ResultCharBudget: datetimeMathResultCharBudget,
			Handler:          datetimeMathHandler,
		},
		{
			Name:             "datetime_format",
			Description:      "Format an RFC3339 timestamp into a compact user-facing string using a stable style, optionally after timezone conversion.",
			Parameters:       json.RawMessage(`{"type":"object","properties":{"timestamp":{"type":"string","description":"RFC3339 timestamp to format."},"style":{"type":"string","enum":["short","long","date_only","time_only","weekday_date"],"description":"Stable output style."},"target_timezone":{"type":"string","description":"Optional IANA timezone to convert into before formatting."},"locale":{"type":"string","description":"Optional future-safe locale hint. Currently English-only formatting is used."}},"required":["timestamp","style"],"additionalProperties":false}`),
			InvocationPolicy: InvocationPolicyDiscretionary,
			ResultCharBudget: datetimeFormatResultCharBudget,
			Handler:          datetimeFormatHandler,
		},
		{
			Name:             "fetch_url_content",
			Description:      "Fetch and extract article-like content from a URL when the user explicitly asks to inspect, explain, summarize, or analyze a link.",
			Parameters:       json.RawMessage(`{"type":"object","properties":{"url":{"type":"string","description":"HTTP or HTTPS URL to fetch"}},"required":["url"],"additionalProperties":false}`),
			InvocationPolicy: InvocationPolicyExplicitRequestOnly,
			ResultCharBudget: fetchURLContentResultCharBudget,
			Handler:          runtime.fetchURLContent,
		},
		{
			Name:             "search_history",
			Description:      "Search the full current chat and current topic in-memory history for evidence snippets that help answer recall questions. Provide a short list of content-word keywords and choose match_mode='all' for narrow recall or 'any' for broader discovery.",
			Parameters:       json.RawMessage(`{"type":"object","properties":{"keywords":{"type":"array","items":{"type":"string"},"minItems":1,"maxItems":8,"description":"Short list of search keywords or short phrases. Words are normalized and matched token-by-token."},"match_mode":{"type":"string","enum":["all","any"],"description":"Whether all keywords must match or any keyword is enough."},"limit":{"type":"integer","minimum":1,"maximum":20,"description":"Maximum number of snippets to return."}},"required":["keywords"],"additionalProperties":false}`),
			InvocationPolicy: InvocationPolicyDiscretionary,
			ResultCharBudget: searchHistoryResultCharBudget,
			Handler:          runtime.searchHistory,
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
			Name:             "get_chat_activity_window",
			Description:      "Summarize the current chat/topic in-memory message cadence with first and last message times, counts, and a rough burstiness label.",
			Parameters:       json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
			InvocationPolicy: InvocationPolicyDiscretionary,
			ResultCharBudget: chatActivityResultCharBudget,
			Handler:          runtime.getChatActivityWindow,
		},
		{
			Name:             "get_history_bounds",
			Description:      "Report exact full-history and recent-history time bounds for the current chat/topic in-memory message scope.",
			Parameters:       json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
			InvocationPolicy: InvocationPolicyDiscretionary,
			ResultCharBudget: historyBoundsResultCharBudget,
			Handler:          runtime.getHistoryBounds,
		},
		{
			Name:             "get_message_thread_context",
			Description:      "Return a compact reply-thread view around the current message, including the linear reply chain and a few direct side replies still available in memory for the current chat/topic.",
			Parameters:       json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
			InvocationPolicy: InvocationPolicyDiscretionary,
			ResultCharBudget: messageThreadResultCharBudget,
			Handler:          runtime.getMessageThreadContext,
		},
		{
			Name:             "send_poll",
			Description:      "Send a regular Telegram poll in the current chat when the user explicitly asks to make a vote or poll.",
			Parameters:       json.RawMessage(`{"type":"object","properties":{"question":{"type":"string","description":"Poll question text"},"options":{"type":"array","items":{"type":"string"},"minItems":2,"maxItems":10,"description":"Poll answer options"},"allows_multiple_answers":{"type":"boolean","description":"Whether voters may choose more than one option"}},"required":["question","options"],"additionalProperties":false}`),
			InvocationPolicy: InvocationPolicyExplicitRequestOnly,
			ResultCharBudget: sendPollResultCharBudget,
			SideEffecting:    true,
			Handler:          runtime.sendPoll,
		},
		{
			Name:             "send_quiz",
			Description:      "Send a Telegram quiz poll in the current chat when the user explicitly asks for a quiz, trivia, or guess-the-answer poll.",
			Parameters:       json.RawMessage(`{"type":"object","properties":{"question":{"type":"string","description":"Quiz question text"},"options":{"type":"array","items":{"type":"string"},"minItems":2,"maxItems":10,"description":"Quiz answer options"},"correct_option_index":{"type":"integer","minimum":0,"description":"0-based index of the correct answer option"},"explanation":{"type":"string","description":"Optional explanation shown after an incorrect answer"}},"required":["question","options","correct_option_index"],"additionalProperties":false}`),
			InvocationPolicy: InvocationPolicyExplicitRequestOnly,
			ResultCharBudget: sendQuizResultCharBudget,
			SideEffecting:    true,
			Handler:          runtime.sendQuiz,
		},
		{
			Name:             "send_dice",
			Description:      "Send a Telegram dice-style animated emoji in the current chat when the user explicitly asks for a dice roll or a small random mini-game.",
			Parameters:       json.RawMessage(`{"type":"object","properties":{"emoji":{"type":"string","description":"Optional Telegram dice emoji: 🎲, 🎯, 🏀, ⚽, 🎳, or 🎰"}},"additionalProperties":false}`),
			InvocationPolicy: InvocationPolicyExplicitRequestOnly,
			ResultCharBudget: sendDiceResultCharBudget,
			SideEffecting:    true,
			Handler:          runtime.sendDice,
		},
		{
			Name:             "list_chat_schedule",
			Description:      "List active reminders for the current chat when the user explicitly asks about reminders or the schedule.",
			Parameters:       json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
			InvocationPolicy: InvocationPolicyExplicitRequestOnly,
			ResultCharBudget: reminderResultCharBudget,
			Handler:          runtime.listChatSchedule,
		},
		{
			Name:             "add_schedule_item",
			Description:      "Create a reminder for the current chat when the user explicitly asks for one. Use get_current_time first when you need to anchor relative time, and ask the user if a calendar-based reminder lacks timezone context.",
			Parameters:       json.RawMessage(`{"type":"object","properties":{"text":{"type":"string","description":"Reminder text to deliver later"},"schedule_type":{"type":"string","enum":["one_shot","interval_days","interval_weeks","weekdays","monthly"],"description":"Reminder schedule type"},"run_at":{"type":"string","description":"RFC3339 timestamp with timezone offset for the first run. Required for one_shot and interval-based reminders."},"timezone":{"type":"string","description":"IANA timezone such as Europe/Moscow. Required for weekday and monthly reminders."},"interval_value":{"type":"integer","minimum":1,"description":"Positive interval count for interval_days and interval_weeks reminders."},"weekdays":{"type":"array","items":{"type":"string","enum":["monday","tuesday","wednesday","thursday","friday","saturday","sunday"]},"minItems":1,"description":"Weekdays used by the weekdays schedule type."},"day_of_month":{"type":"integer","minimum":1,"maximum":31,"description":"Day of month used by the monthly schedule type."},"time_of_day":{"type":"string","description":"24-hour local time in HH:MM format for weekday and monthly schedules."}},"required":["text","schedule_type"],"additionalProperties":false}`),
			InvocationPolicy: InvocationPolicyExplicitRequestOnly,
			ResultCharBudget: reminderResultCharBudget,
			SideEffecting:    true,
			Handler:          runtime.addScheduleItem,
		},
		{
			Name:             "remove_schedule_item",
			Description:      "Remove an active reminder from the current chat by stable reminder ID when the user explicitly asks to cancel one.",
			Parameters:       json.RawMessage(`{"type":"object","properties":{"reminder_id":{"type":"string","description":"Stable reminder identifier returned by list_chat_schedule"}},"required":["reminder_id"],"additionalProperties":false}`),
			InvocationPolicy: InvocationPolicyExplicitRequestOnly,
			ResultCharBudget: reminderResultCharBudget,
			SideEffecting:    true,
			Handler:          runtime.removeScheduleItem,
		},
	}

	if runtime.searcher != nil {
		definitions = append(definitions, Definition{
			Name:             "search_web",
			Description:      "Search the public web for external facts or fresh information when current chat history is insufficient.",
			Parameters:       json.RawMessage(`{"type":"object","properties":{"query":{"type":"string","description":"Search query text"},"max_results":{"type":"integer","minimum":1,"maximum":10,"description":"Maximum number of results to return."}},"required":["query"],"additionalProperties":false}`),
			InvocationPolicy: InvocationPolicyDiscretionaryPaid,
			ResultCharBudget: fetchURLContentResultCharBudget,
			Handler:          runtime.searchWeb,
		})
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

// Register adds a new tool definition to the registry and includes it in the
// default set returned to the LLM. Re-registering an existing name replaces
// the prior definition; this is intentionally permissive so the MCP adapter
// can re-publish a tool after a reconnect without first having to deregister
// it (Deregister is intentionally absent — see plan for the list_changed
// follow-up).
//
// Callers are responsible for ensuring the namespaced name does not collide
// with a built-in tool; the MCP translator enforces this at translation time.
func (r *Registry) Register(d Definition) {
	if r.definitions == nil {
		r.definitions = make(map[string]Definition)
	}
	if r.defaults == nil {
		r.defaults = make([]string, 0)
	}
	if _, exists := r.definitions[d.Name]; !exists {
		r.defaults = append(r.defaults, d.Name)
	}
	r.definitions[d.Name] = d
}

func isValidToolURL(raw string) bool {
	parsed, err := url.ParseRequestURI(raw)
	if err != nil {
		return false
	}

	return slices.Contains([]string{"http", "https"}, strings.ToLower(parsed.Scheme))
}
