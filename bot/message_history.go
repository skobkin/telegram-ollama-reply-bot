package bot

import (
	"log/slog"

	"github.com/getsentry/sentry-go"
	t "github.com/mymmrac/telego"
)

type MessageData struct {
	Name          string
	Username      string
	Text          string
	IsMe          bool
	IsUserRequest bool
	ReplyTo       *MessageData
	HasImage      bool
	Image         string
	chatID        int64
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

func (b *Bot) saveChatMessageToHistory(msgData MessageData) {
	chatId := msgData.chatID

	_, ok := b.history[chatId]
	if !ok {
		b.history[chatId] = NewMessageHistory(b.cfg.HistoryLength)
	}

	b.history[chatId].Push(msgData)
}

func (b *Bot) saveBotReplyToHistory(replyTo t.Message, text string) {
	chatId := replyTo.Chat.ID

	slog.Info(
		"bot:history: saving bot reply",
		"chat", chatId,
		"to_id", replyTo.From.ID,
		"to_name", replyTo.From.FirstName,
		"text", text,
	)

	_, ok := b.history[chatId]
	if !ok {
		b.history[chatId] = NewMessageHistory(b.cfg.HistoryLength)
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

func (b *Bot) tgUserMessageToMessageData(message t.Message, isUserRequest bool) MessageData {
	msgData := MessageData{
		Name:          message.From.FirstName,
		Username:      message.From.Username,
		Text:          message.Text,
		IsMe:          false,
		IsUserRequest: isUserRequest,
		HasImage:      false,
		Image:         "",
		chatID:        message.Chat.ID,
	}

	if len(message.Photo) > 0 {
		slog.Debug("Processing message photo", "message_id", message.MessageID, "photo_sizes", len(message.Photo), "photo", message.Photo)

		msgData.HasImage = true
		// Get the highest quality photo (last in the array)
		// TODO: add image size selection according to size limit (needs to be implemented too)
		photo := message.Photo[len(message.Photo)-1]
		description, err := b.describeImage(photo)

		if err != nil {
			slog.Error("bot: Failed to describe image", "error", err, "message_id", message.MessageID)
			sentry.CaptureException(err)
		} else if description != "" {
			slog.Debug("bot: Got image description", "description", description, "message_id", message.MessageID)
			msgData.Image = description
		} else {
			slog.Info("bot: Image recognition ended with empty description", "message_id", message.MessageID)
		}
	}

	if message.ReplyToMessage != nil {
		replyData := b.tgUserMessageToMessageData(*message.ReplyToMessage, false)
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

func (b *Bot) ResetChatHistory(chatId int64) {
	_, ok := b.history[chatId]
	if !ok {
		slog.Debug("bot: Chat ID not found in history", "chat_id", chatId)
		return
	}

	slog.Info("bot: Resetting chat history", "chat_id", chatId)
	b.history[chatId] = NewMessageHistory(b.cfg.HistoryLength)
}
