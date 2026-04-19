package llmcontext

import (
	"context"
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
	history state.ConversationStore
	hydrate HydrateFunc
}

func NewReplyBuilder(history state.ConversationStore, hydrate HydrateFunc) *ReplyBuilder {
	if history == nil {
		panic("history store is required")
	}
	if hydrate == nil {
		panic("hydrate function is required")
	}

	return &ReplyBuilder{
		history: history,
		hydrate: hydrate,
	}
}

func (b *ReplyBuilder) BuildReplyContext(ctx context.Context, input ReplyInput) llm.ChatReplyContext {
	snapshot := b.history.Snapshot(input.Scope)
	history := b.hydrate(ctx, snapshot.Messages)
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

	return sb.String()
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
