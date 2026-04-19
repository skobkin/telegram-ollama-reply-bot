package bot

import (
	"context"
	"strings"
	"time"

	"telegram-ollama-reply-bot/internal/llm"
	"telegram-ollama-reply-bot/internal/llmcontext"
	"telegram-ollama-reply-bot/internal/state"

	"github.com/getsentry/sentry-go"
	t "github.com/mymmrac/telego"
)

func scopeFromMessage(message t.Message) state.ConversationScope {
	return state.ConversationScope{
		ChatID:  message.Chat.ID,
		TopicID: message.MessageThreadID,
	}
}

func (b *Bot) saveChatMessageToHistory(msg state.Message) {
	b.history.AppendMessage(state.ConversationScope{
		ChatID:  msg.ChatID,
		TopicID: msg.TopicID,
	}, msg)
}

func (b *Bot) saveBotReplyToHistory(ctx context.Context, replyTo t.Message, text string) {
	scope := scopeFromMessage(replyTo)
	b.loggerFromContext(ctx).Debug("saving bot reply to history", "chat_id", scope.ChatID, "topic_id", scope.TopicID, "reply_length", len(text))

	botName := strings.TrimSpace(b.me.FirstName + " " + b.me.LastName)
	if botName == "" {
		botName = b.me.Username
	}

	msg := state.Message{
		Name:      botName,
		Username:  b.me.Username,
		Text:      text,
		IsMe:      true,
		ChatID:    scope.ChatID,
		TopicID:   scope.TopicID,
		CreatedAt: time.Now().UTC(),
	}

	if replyTo.ReplyToMessage != nil {
		replyMessage := *replyTo.ReplyToMessage
		replyScope := scopeFromMessage(replyMessage)
		replyData := b.tgUserMessageToMessageData(replyMessage, false)
		replyData.ChatID = replyScope.ChatID
		replyData.TopicID = replyScope.TopicID
		msg.ReplyTo = &replyData
	}

	b.history.AppendMessage(scope, msg)
}

func (b *Bot) tgUserMessageToMessageData(message t.Message, isUserRequest bool) state.Message {
	msg := state.Message{
		IsUserRequest: isUserRequest,
		ChatID:        message.Chat.ID,
		TopicID:       message.MessageThreadID,
		MessageID:     message.MessageID,
		CreatedAt:     time.Now().UTC(),
		Text:          message.Text,
	}

	if message.Date != 0 {
		msg.CreatedAt = time.Unix(message.Date, 0).UTC()
	}
	if message.From != nil {
		msg.Name = message.From.FirstName
		msg.Username = message.From.Username
		msg.FromID = message.From.ID
	}

	if len(message.Photo) > 0 {
		b.loggerFromContext(b.ctx).Debug("message contains photo", "message_id", message.MessageID, "photo_sizes", len(message.Photo))

		photo := message.Photo[len(message.Photo)-1]
		msg.HasImage = true
		msg.ImageMeta = &state.ImageMeta{
			FileID:       photo.FileID,
			FileUniqueID: photo.FileUniqueID,
			Width:        photo.Width,
			Height:       photo.Height,
			FileSize:     photo.FileSize,
		}
	}

	if message.ReplyToMessage != nil {
		replyData := b.tgUserMessageToMessageData(*message.ReplyToMessage, false)
		msg.ReplyTo = &replyData
	}

	return msg
}

func (b *Bot) ResetChatHistory(chatID int64) {
	b.loggerFromContext(b.ctx).Info("resetting chat history", "chat_id", chatID)
	b.history.ResetChat(chatID)
}

func (b *Bot) maybeSummarizeHistory(ctx context.Context, message t.Message) {
	scope := scopeFromMessage(message)
	snapshot := b.history.Snapshot(scope)
	if len(snapshot.Messages) == 0 {
		return
	}

	limit := b.cfg.UncompressedHistoryLimit
	threshold := b.cfg.HistorySummaryThreshold
	if limit <= 0 {
		return
	}

	historyLen := len(snapshot.Messages)
	unsummarized := historyLen - snapshot.SummarizedUntil
	if unsummarized <= limit+threshold {
		return
	}

	end := historyLen - limit
	start := snapshot.SummarizedUntil
	if start >= end {
		return
	}
	slice := snapshot.Messages[start:end]
	if len(slice) == 0 {
		b.history.SetEarlierSummary(scope, snapshot.EarlierSummary, end)

		return
	}

	workCtx, cancel := b.withProcessingDeadline(ctx)
	defer cancel()

	slice = b.hydrateMessagesWithImageDescriptions(workCtx, slice)

	text := llmcontext.RenderMessagesPlainText(slice)
	if snapshot.EarlierSummary != "" {
		text = "Earlier conversation summary:\n" + snapshot.EarlierSummary + "\n\nRecent messages:\n" + text
	}

	summary, usage, err := b.llm.Summarize(workCtx, llm.PromptScope{ChatID: scope.ChatID}, text, "")
	if err != nil {
		b.loggerFromContext(workCtx).Error("failed to summarize history", "error", err, "chat_id", scope.ChatID, "topic_id", scope.TopicID)
		sentry.CaptureException(err)

		return
	}
	if usage != nil {
		b.stats.AddUsage(usage.PromptTokens, usage.CompletionTokens, usage.TotalTokens, usage.Cost)
	}
	b.history.SetEarlierSummary(scope, summary, end)
}
