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

type MessageRingBuffer struct {
	messages []MessageData
	capacity int
}

func NewMessageBuffer(capacity int) *MessageRingBuffer {
	return &MessageRingBuffer{
		messages: make([]MessageData, 0, capacity),
		capacity: capacity,
	}
}

func (b *MessageRingBuffer) Push(element MessageData) {
	if len(b.messages) >= b.capacity {
		b.messages = b.messages[1:]
	}

	b.messages = append(b.messages, element)
}

func (b *MessageRingBuffer) GetAll() []MessageData {
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
		b.history[chatId] = NewMessageBuffer(HistoryLength)
	}

	msgData := tgUserMessageToMessageData(message)

	b.history[chatId].Push(msgData)
}

func (b *Bot) saveBotReplyToHistory(message *telego.Message, text string) {
	chatId := message.Chat.ID

	slog.Info(
		"history-reply-save",
		"chat", chatId,
		"to_id", message.From.ID,
		"to_name", message.From.FirstName,
		"text", text,
	)

	_, ok := b.history[chatId]
	if !ok {
		b.history[chatId] = NewMessageBuffer(HistoryLength)
	}

	msgData := MessageData{
		Name:     b.profile.Name,
		Username: b.profile.Username,
		Text:     text,
		IsMe:     true,
	}

	if message.ReplyToMessage != nil {
		replyMessage := message.ReplyToMessage

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

func tgUserMessageToMessageData(message *telego.Message) MessageData {
	msgData := MessageData{
		Name:     message.From.FirstName,
		Username: message.From.Username,
		Text:     message.Text,
		IsMe:     false,
	}

	if message.ReplyToMessage != nil {
		replyData := tgUserMessageToMessageData(message.ReplyToMessage)
		msgData.ReplyTo = &replyData
	}

	return msgData
}

func (b *Bot) getChatHistory(chatId int64) []MessageData {
	_, ok := b.history[chatId]
	if !ok {
		return make([]MessageData, 0)
	}

	return b.history[chatId].GetAll()
}
