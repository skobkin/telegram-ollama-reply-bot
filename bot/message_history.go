package bot

import (
	"github.com/mymmrac/telego"
	"log/slog"
)

const HistoryLength = 150

type MessageData struct {
	Name          string
	Username      string
	Text          string
	IsMe          bool
	IsUserRequest bool
	ReplyTo       *MessageData
}

type MessageHistory struct {
	messages []MessageData
	capacity int
}

func NewMessageHistory(capacity int) *MessageHistory {
	return &MessageHistory{
		messages: make([]MessageData, 0, capacity),
		capacity: capacity,
	}
}

func (b *MessageHistory) Push(element MessageData) {
	if len(b.messages) >= b.capacity {
		b.messages = b.messages[1:]
	}

	b.messages = append(b.messages, element)
}

func (b *MessageHistory) GetAll() []MessageData {
	return b.messages
}

func (b *Bot) saveChatMessageToHistory(message *telego.Message) {
	chatId := message.Chat.ID

	slog.Info(
		"history-message-save",
		"chat", chatId,
		"from_id", message.From.ID,
		"from_name", message.From.FirstName,
		"text", message.Text,
	)

	_, ok := b.history[chatId]
	if !ok {
		b.history[chatId] = NewMessageHistory(HistoryLength)
	}

	msgData := tgUserMessageToMessageData(message, false)

	b.history[chatId].Push(msgData)
}

func (b *Bot) saveBotReplyToHistory(replyTo *telego.Message, text string) {
	chatId := replyTo.Chat.ID

	slog.Info(
		"history-reply-save",
		"chat", chatId,
		"to_id", replyTo.From.ID,
		"to_name", replyTo.From.FirstName,
		"text", text,
	)

	_, ok := b.history[chatId]
	if !ok {
		b.history[chatId] = NewMessageHistory(HistoryLength)
	}

	msgData := MessageData{
		Name:     b.profile.Name,
		Username: b.profile.Username,
		Text:     text,
		IsMe:     true,
	}

	if replyTo.ReplyToMessage != nil {
		replyMessage := replyTo.ReplyToMessage

		msgData.ReplyTo = &MessageData{
			Name:     replyMessage.From.FirstName,
			Username: replyMessage.From.Username,
			Text:     replyMessage.Text,
			IsMe:     false,
			ReplyTo:  nil,
		}
	}

	b.history[chatId].Push(msgData)
}

func tgUserMessageToMessageData(message *telego.Message, isUserRequest bool) MessageData {
	msgData := MessageData{
		Name:          message.From.FirstName,
		Username:      message.From.Username,
		Text:          message.Text,
		IsMe:          false,
		IsUserRequest: isUserRequest,
	}

	if message.ReplyToMessage != nil {
		replyData := tgUserMessageToMessageData(message.ReplyToMessage, false)
		msgData.ReplyTo = &replyData
	}

	return msgData
}

func (b *Bot) getChatHistory(chatId int64) []MessageData {
	_, ok := b.history[chatId]
	if !ok {
		slog.Debug("bot: Chat ID not found in history", "chat_id", chatId)

		return make([]MessageData, 0)
	}

	return b.history[chatId].GetAll()
}
