package bot

import (
	"context"
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
	ImageMeta     *ImageMeta
	chatID        int64
}

type ImageMeta struct {
	FileID       string
	FileUniqueID string
	Width        int
	Height       int
	FileSize     int
}

func (i *ImageMeta) cacheKey() string {
	if i == nil {
		return ""
	}
	if i.FileUniqueID != "" {
		return i.FileUniqueID
	}

	return i.FileID
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
	chatID := msgData.chatID

	_, ok := b.history[chatID]
	if !ok {
		b.history[chatID] = NewMessageHistory(b.cfg.HistoryLength)
	}

	b.history[chatID].Push(msgData)
}

func (b *Bot) saveBotReplyToHistory(ctx context.Context, replyTo t.Message, text string) {
	chatID := replyTo.Chat.ID
	b.loggerFromContext(ctx).Debug("saving bot reply to history", "chat_id", chatID, "reply_length", len(text))

	_, ok := b.history[chatID]
	if !ok {
		b.history[chatID] = NewMessageHistory(b.cfg.HistoryLength)
	}

	botName := strings.TrimSpace(b.me.FirstName + " " + b.me.LastName)
	if botName == "" {
		botName = b.me.Username
	}
	botUsername := b.me.Username

	msgData := MessageData{
		Name:     botName,
		Username: botUsername,
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

	b.history[chatID].Push(msgData)
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
		b.loggerFromContext(b.ctx).Debug("message contains photo", "message_id", message.MessageID, "photo_sizes", len(message.Photo))

		msgData.HasImage = true
		photo := message.Photo[len(message.Photo)-1]
		msgData.ImageMeta = &ImageMeta{
			FileID:       photo.FileID,
			FileUniqueID: photo.FileUniqueID,
			Width:        photo.Width,
			Height:       photo.Height,
			FileSize:     photo.FileSize,
		}
	}

	if message.ReplyToMessage != nil {
		replyData := b.tgUserMessageToMessageData(*message.ReplyToMessage, false)
		msgData.ReplyTo = &replyData
	}

	return msgData
}

func (b *Bot) getChatHistory(chatID int64) []MessageData {
	_, ok := b.history[chatID]
	if !ok {
		b.loggerFromContext(b.ctx).Debug("chat history not found", "chat_id", chatID)

		return make([]MessageData, 0)
	}

	return b.history[chatID].GetAll()
}

func (b *Bot) ResetChatHistory(chatID int64) {
	_, ok := b.history[chatID]
	if !ok {
		b.loggerFromContext(b.ctx).Debug("chat history not found", "chat_id", chatID)

		return
	}

	b.loggerFromContext(b.ctx).Info("resetting chat history", "chat_id", chatID)
	b.history[chatID] = NewMessageHistory(b.cfg.HistoryLength)
}

func (b *Bot) maybeSummarizeHistory(ctx context.Context, chatID int64) {
	mh, ok := b.history[chatID]
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

	workCtx, cancel := b.withProcessingDeadline(ctx)
	defer cancel()

	b.ensureHistoryMessagesImageDescriptions(workCtx, chatID)

	text := historyToPlainText(slice)

	if mh.earlierSummary.Text != "" {
		// TODO: introduce a dedicated llm method for history summarization
		// that provides a consistent presentation for earlier and recent messages
		text = "Earlier conversation summary:\n" + mh.earlierSummary.Text + "\n\nRecent messages:\n" + text
	}

	summary, usage, err := b.llm.Summarize(workCtx, text, "")
	if err != nil {
		b.loggerFromContext(workCtx).Error("failed to summarize history", "error", err, "chat_id", chatID)
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
