package bot

import (
	"github.com/mymmrac/telego"
	"log/slog"
)

const HistoryLength = 50

type MessageRingBuffer struct {
	messages []Message
	capacity int
}

func NewMessageBuffer(capacity int) *MessageRingBuffer {
	return &MessageRingBuffer{
		messages: make([]Message, 0, capacity),
		capacity: capacity,
	}
}

func (b *MessageRingBuffer) Push(element Message) {
	if len(b.messages) >= b.capacity {
		b.messages = b.messages[1:]
	}

	b.messages = append(b.messages, element)
}

func (b *MessageRingBuffer) GetAll() []Message {
	return b.messages
}

type Message struct {
	Name string
	Text string
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

	b.history[chatId].Push(Message{
		Name: message.From.FirstName,
		Text: message.Text,
	})
}

func (b *Bot) saveBotReplyToHistory(message *telego.Message, reply string) {
	chatId := message.Chat.ID

	slog.Info(
		"history-reply-save",
		"chat", chatId,
		"to_id", message.From.ID,
		"to_name", message.From.FirstName,
		"text", reply,
	)

	_, ok := b.history[chatId]
	if !ok {
		b.history[chatId] = NewMessageBuffer(HistoryLength)
	}

	b.history[chatId].Push(Message{
		Name: b.profile.Username,
		Text: reply,
	})
}

func (b *Bot) getChatHistory(chatId int64) []Message {
	_, ok := b.history[chatId]
	if !ok {
		return make([]Message, 0)
	}

	return b.history[chatId].GetAll()
}
