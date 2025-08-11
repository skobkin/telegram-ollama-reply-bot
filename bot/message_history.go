package bot

import (
	"context"
	"log/slog"
	"strings"

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

type EarlierSummary struct {
	Text string
	// SummarizedUntil is the index of the first message that has not yet been
	// summarized into Text.
	SummarizedUntil int
}

type MessageHistory struct {
	messages       []MessageData
	capacity       int
	earlierSummary EarlierSummary
}

func NewMessageHistory(capacity int) *MessageHistory {
	return &MessageHistory{
		messages:       make([]MessageData, 0, capacity),
		capacity:       capacity,
		earlierSummary: EarlierSummary{},
	}
}

func (b *MessageHistory) Push(element MessageData) {
	if len(b.messages) >= b.capacity {
		b.messages = b.messages[1:]
		if b.earlierSummary.SummarizedUntil > 0 {
			b.earlierSummary.SummarizedUntil--
		}
	}

	b.messages = append(b.messages, element)
}

func (b *MessageHistory) GetAll() []MessageData {
	return b.messages
}

func (b *MessageHistory) EarlierSummary() string {
	return b.earlierSummary.Text
}

func (b *MessageHistory) SetEarlierSummary(sum string) {
	b.earlierSummary.Text = sum
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

func (b *Bot) maybeSummarizeHistory(chatId int64) {
	mh, ok := b.history[chatId]
	if !ok {
		return
	}

	limit := b.cfg.UncompressedHistoryLimit
	threshold := b.cfg.HistorySummaryThreshold
	if limit <= 0 {
		return
	}

	historyLen := len(mh.messages)
	unsummarized := historyLen - mh.earlierSummary.SummarizedUntil
	if unsummarized <= limit+threshold {
		return
	}

	end := historyLen - limit
	start := mh.earlierSummary.SummarizedUntil
	if start >= end {
		return
	}
	slice := mh.messages[start:end]
	if len(slice) == 0 {
		mh.earlierSummary.SummarizedUntil = end
		return
	}
	text := historyToPlainText(slice)

	if mh.earlierSummary.Text != "" {
		// TODO: introduce a dedicated llm method for history summarization
		// that provides a consistent presentation for earlier and recent messages
		text = "Earlier conversation summary:\n" + mh.earlierSummary.Text + "\n\nRecent messages:\n" + text
	}

	ctx, cancel := context.WithTimeout(b.ctx, b.cfg.LlmRequestTimeout)
	defer cancel()
	summary, usage, err := b.llm.Summarize(ctx, text, "")
	if err != nil {
		slog.Error("bot: failed to summarize history", "error", err, "chat", chatId)
		sentry.CaptureException(err)
		return
	}
	if usage != nil {
		b.stats.AddUsage(usage.PromptTokens, usage.CompletionTokens, usage.TotalTokens, usage.Cost)
	}
	mh.SetEarlierSummary(summary)
	mh.earlierSummary.SummarizedUntil = end
}

func historyToPlainText(history []MessageData) string {
	var sb strings.Builder
	for _, msg := range history {
		sb.WriteString(messageDataToPlainText(msg))
		sb.WriteString("\n")
	}
	return sb.String()
}

func messageDataToPlainText(msg MessageData) string {
	var sb strings.Builder
	if msg.ReplyTo != nil {
		sb.WriteString("> ")
		sb.WriteString(messageDataToPlainText(*msg.ReplyTo))
		sb.WriteString("\n")
	}
	sb.WriteString(presentMessage(msg))
	return sb.String()
}

func presentMessage(msg MessageData) string {
	result := msg.Name
	if msg.Username != "" {
		result += " (@" + msg.Username + ")"
	}
	result += ": "
	if msg.HasImage {
		if msg.Image != "" {
			result += "[Image: " + msg.Image + "] "
		} else {
			result += "[Image] "
		}
	}
	result += msg.Text
	return result
}
