package llmcontext

import (
	"context"
	"fmt"
	"strings"

	"telegram-ollama-reply-bot/internal/llm"
	"telegram-ollama-reply-bot/internal/state"
)

type TriggerKind string

const (
	TriggerMention TriggerKind = "mention"
	TriggerReply   TriggerKind = "reply"
	TriggerPrivate TriggerKind = "private"
)

type HydrateFunc func(context.Context, []state.Message) []state.Message

type ReplyInput struct {
	Chat           ChatContext
	User           UserContext
	Scope          state.ConversationScope
	CurrentMessage state.Message
	Trigger        TriggerKind
}

type ChatContext struct {
	Title       string
	Description string
	Type        string
}

type UserContext struct {
	Username  string
	FirstName string
	LastName  string
	IsPremium bool
}

type ReplyBuilder struct {
	history     state.ConversationStore
	hydrate     HydrateFunc
	recentLimit int
	// summaryTailLimit bounds the raw tail forwarded while a summary exists.
	// Normal operation keeps the unsummarized tail within the summarization
	// window, but a cleared-summary rebuild commits chunks incrementally, so
	// a partial summary can leave a long raw middle — forwarding all of it
	// to the reply model could exceed its context window.
	summaryTailLimit int
}

func NewReplyBuilder(history state.ConversationStore, hydrate HydrateFunc, recentLimit, summaryTailLimit int) *ReplyBuilder {
	if history == nil {
		panic("history store is required")
	}
	if hydrate == nil {
		panic("hydrate function is required")
	}
	if recentLimit < 0 {
		panic("recent history limit must not be negative")
	}
	if summaryTailLimit < 0 {
		panic("summary tail limit must not be negative")
	}

	return &ReplyBuilder{
		history:          history,
		hydrate:          hydrate,
		recentLimit:      recentLimit,
		summaryTailLimit: summaryTailLimit,
	}
}

func (b *ReplyBuilder) BuildReplyContext(ctx context.Context, input ReplyInput) llm.ChatReplyContext {
	snapshot := b.history.Snapshot(input.Scope)
	start := snapshot.SummaryMessageCount
	if start < 0 {
		start = 0
	}
	if start > len(snapshot.Messages) {
		start = len(snapshot.Messages)
	}
	if snapshot.EarlierSummary == "" {
		// No summary: bound to the recent window when one is configured.
		// With summarization disabled entirely (limit 0) the full history
		// stays available — nothing omitted here ever returns via a summary.
		if b.recentLimit > 0 {
			start = len(snapshot.Messages) - b.recentLimit
			if start < 0 {
				start = 0
			}
		}
	} else if b.summaryTailLimit > 0 && len(snapshot.Messages)-start > b.summaryTailLimit {
		// Partial summary mid-rebuild: the committed prefix is covered by
		// the summary, so keep the reply context on the newest tail instead
		// of forwarding the whole unsummarized middle. The middle returns as
		// summarization catches up.
		start = len(snapshot.Messages) - b.summaryTailLimit
	}

	history := b.hydrate(ctx, snapshot.Messages[start:])
	current := hydrateOne(ctx, input.CurrentMessage, b.hydrate)

	result := llm.ChatReplyContext{
		SystemHint:     renderSystemHint(input),
		EarlierSummary: snapshot.EarlierSummary,
		History:        messagesToLLM(history),
		UserMessage:    messageToLLM(current),
	}

	return result
}

func RenderMessagesPlainText(messages []state.Message) string {
	var sb strings.Builder

	for _, msg := range messages {
		sb.WriteString(renderMessagePlainText(msg))
		sb.WriteString("\n")
	}

	return sb.String()
}

func hydrateOne(ctx context.Context, msg state.Message, hydrate HydrateFunc) state.Message {
	hydrated := hydrate(ctx, []state.Message{msg})
	if len(hydrated) == 0 {
		return msg
	}

	return hydrated[0]
}

func renderSystemHint(input ReplyInput) string {
	var lines []string

	lines = append(lines, "Context hints:")
	lines = append(lines, "- Chat type: "+quotedOrUnknown(input.Chat.Type)+".")

	if input.Chat.Title != "" {
		lines = append(lines, "- Chat title: "+quoteValue(input.Chat.Title)+".")
	}

	if input.Chat.Description != "" {
		lines = append(lines, "- Chat description: "+quoteValue(input.Chat.Description)+".")
	}

	if input.Trigger != "" {
		lines = append(lines, "- Trigger: "+string(input.Trigger)+".")
	}

	if input.Scope.TopicID != 0 {
		lines = append(lines, "- The current request is inside a Telegram topic. Use only this topic's context.")
	}

	switch input.Chat.Type {
	case "group", "supergroup":
		lines = append(lines, "- In group chats, reply only to the addressed user and current topic. Avoid summarizing the whole chat unless asked.")
	}

	lines = append(lines, "- User profile:")
	if input.User.FirstName != "" {
		lines = append(lines, "  First name: "+quoteValue(input.User.FirstName)+".")
	}
	if input.User.LastName != "" {
		lines = append(lines, "  Last name: "+quoteValue(input.User.LastName)+".")
	}
	if input.User.Username != "" {
		lines = append(lines, "  Username: @"+input.User.Username+".")
	}

	return strings.Join(lines, "\n")
}

func messagesToLLM(messages []state.Message) []llm.Message {
	if len(messages) == 0 {
		return make([]llm.Message, 0)
	}

	result := make([]llm.Message, 0, len(messages))
	for _, msg := range messages {
		result = append(result, messageToLLM(msg))
	}

	return result
}

func messageToLLM(message state.Message) llm.Message {
	role := llm.RoleUser
	if message.IsMe {
		role = llm.RoleAssistant
	}

	text := renderMessageForModel(message)

	return llm.Message{
		Role: role,
		Parts: []llm.Part{
			{Type: llm.PartTypeText, Text: text},
		},
	}
}

func renderMessageForModel(message state.Message) string {
	if message.IsMe {
		return renderAssistantMessage(message)
	}

	return renderMessagePlainText(message)
}

func renderAssistantMessage(message state.Message) string {
	var sb strings.Builder

	if message.HasImage {
		if message.Image != "" {
			sb.WriteString("[Image: ")
			sb.WriteString(message.Image)
			sb.WriteString("] ")
		} else {
			sb.WriteString("[Image] ")
		}
	}

	sb.WriteString(message.Text)
	sb.WriteString(renderReactionsSuffix(message.Reactions))

	return sb.String()
}

// renderReactionsSuffix renders the feedback users left on a bot message so
// the model can take it into account, e.g. "\n[reactions: 👍 Alice, 👎 Bob]".
func renderReactionsSuffix(reactions []state.Reaction) string {
	if len(reactions) == 0 {
		return ""
	}

	parts := make([]string, 0, len(reactions))
	for _, reaction := range reactions {
		parts = append(parts, reaction.Emoji+" "+reactionDisplayName(reaction))
	}

	return "\n[reactions: " + strings.Join(parts, ", ") + "]"
}

func reactionDisplayName(reaction state.Reaction) string {
	if name := strings.TrimSpace(reaction.Name); name != "" {
		return name
	}
	if reaction.Username != "" {
		return "@" + reaction.Username
	}

	return fmt.Sprintf("user #%d", reaction.UserID)
}

func renderMessagePlainText(message state.Message) string {
	var sb strings.Builder

	if message.ReplyTo != nil {
		sb.WriteString("> ")
		sb.WriteString(renderMessagePlainText(*message.ReplyTo))
		sb.WriteString("\n")
	}

	sb.WriteString(presentMessage(message))

	return sb.String()
}

func presentMessage(message state.Message) string {
	var sb strings.Builder

	sb.WriteString(message.Name)
	if message.Username != "" {
		sb.WriteString(" (@")
		sb.WriteString(message.Username)
		sb.WriteString(")")
	}
	sb.WriteString(": ")

	if message.HasImage {
		if message.Image != "" {
			sb.WriteString("[Image: ")
			sb.WriteString(message.Image)
			sb.WriteString("] ")
		} else {
			sb.WriteString("[Image] ")
		}
	}

	sb.WriteString(message.Text)
	sb.WriteString(renderReactionsSuffix(message.Reactions))

	return sb.String()
}

func quoteValue(value string) string {
	return `"` + value + `"`
}

func quotedOrUnknown(value string) string {
	if value == "" {
		return `"unknown"`
	}

	return quoteValue(value)
}
