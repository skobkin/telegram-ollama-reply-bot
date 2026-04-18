package llm

import (
	"strings"

	"github.com/sashabaranov/go-openai"
)

type RequestContext struct {
	Empty bool
	User  UserContext
	Chat  ChatContext
}

type UserContext struct {
	Username  string
	FirstName string
	LastName  string
	IsPremium bool
}

type ChatContext struct {
	Title          string
	Description    string
	Type           string
	History        []ChatMessage
	EarlierSummary string
}

type ChatMessage struct {
	Name          string
	Username      string
	Text          string
	IsMe          bool
	IsUserRequest bool
	HasImage      bool
	Image         string
	ReplyTo       *ChatMessage
}

func (c RequestContext) Prompt() string {
	if c.Empty {
		return ""
	}

	prompt := ""

	prompt += "The type of chat you're in is \"" + c.Chat.Type + "\". "

	if c.Chat.Type == "group" || c.Chat.Type == "supergroup" {
		prompt += "Please consider that there are several users in this chat type who may discuss several unrelated " +
			"topics. Try to respond only about the topic you were asked about and only to the user who asked you. " +
			"Avoid summarizing an entire chat history unless directly asked. " +
			"Just answer to the person who sent you the message."
	}

	if c.Chat.Title != "" {
		prompt += "\nChat is called \"" + c.Chat.Title + "\". "
	}
	if c.Chat.Description != "" {
		prompt += "Chat description is \"" + c.Chat.Description + "\". "
	}

	prompt += "\nProfile of the user who mentioned you in the chat:\n" +
		"First name: \"" + c.User.FirstName + "\"\n"
	if c.User.Username != "" {
		prompt += "Username: @" + c.User.Username + ".\n"
	}
	if c.User.LastName != "" {
		prompt += "Last name: \"" + c.User.LastName + "\"\n"
	}
	//if c.User.IsPremium {
	//      prompt += "Telegram Premium subscription: active."
	//}

	return prompt
}

func chatMessageToOpenAiChatCompletionMessage(message ChatMessage) openai.ChatCompletionMessage {
	var msgRole string

	switch {
	case message.IsMe:
		msgRole = openai.ChatMessageRoleAssistant
	case message.IsUserRequest:
		msgRole = openai.ChatMessageRoleUser
	default:
		msgRole = openai.ChatMessageRoleUser
	}

	var msgText string
	if message.IsMe {
		if message.HasImage {
			if message.Image != "" {
				msgText += "[Image: " + message.Image + "] "
			} else {
				msgText += "[Image] "
			}
		}
		msgText += message.Text
	} else {
		msgText = chatMessageToText(message)
	}

	return openai.ChatCompletionMessage{
		Role:    msgRole,
		Content: msgText,
	}
}

func chatMessageToText(message ChatMessage) string {
	var msgText string

	if message.ReplyTo != nil {
		msgText = "> " + presentUserMessageAsText(*message.ReplyTo) + "\n"
	}
	msgText += presentUserMessageAsText(message)

	return msgText
}

func presentUserMessageAsText(message ChatMessage) string {
	result := message.Name
	if message.Username != "" {
		result += " (@" + message.Username + ")"
	}
	result += ": "

	if message.HasImage {
		if message.Image != "" {
			result += "[Image: " + message.Image + "] "
		} else {
			result += "[Image] "
		}
	}

	result += message.Text

	return result
}

func chatHistoryToPlainText(history []ChatMessage) string {
	var sb strings.Builder
	for _, msg := range history {
		sb.WriteString(chatMessageToText(msg))
		sb.WriteString("\n")
	}
	return sb.String()
}
