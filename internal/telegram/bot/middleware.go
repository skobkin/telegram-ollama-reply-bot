package bot

import (
	"telegram-ollama-reply-bot/internal/logging"

	t "github.com/mymmrac/telego"
	th "github.com/mymmrac/telego/telegohandler"
)

// requestContextMessageDataKey is the context key for storing processed message data in request context
const requestContextMessageDataKey = "message_data"

func (b *Bot) requestLogger(ctx *th.Context, update t.Update) error {
	requestID, err := logging.NewRequestID()
	if err != nil {
		return err
	}

	baseLogger := b.loggerFromContext(ctx.Context())
	requestLogger := baseLogger.With("update_id", update.UpdateID)

	if update.Message != nil {
		requestLogger = requestLogger.With(
			"chat_id", update.Message.Chat.ID,
			"message_id", update.Message.MessageID,
			"chat_type", update.Message.Chat.Type,
		)
		if update.Message.From != nil {
			requestLogger = requestLogger.With("from_id", update.Message.From.ID)
		}
	}

	requestCtx := logging.WithRequestLogger(ctx.Context(), requestLogger, requestID)
	ctx = ctx.WithContext(requestCtx)
	b.handlerLogger(ctx).Debug("request context initialized")

	return ctx.Next(update)
}

func (b *Bot) chatTypeStatsCounter(ctx *th.Context, update t.Update) error {
	message := update.Message
	logger := b.handlerLogger(ctx)

	if message == nil {
		logger.Debug("stats middleware skipped update without message")

		return ctx.Next(update)
	}

	switch message.Chat.Type {
	case t.ChatTypeGroup, t.ChatTypeSupergroup:
		if b.isMentionOfMe(*message) || b.isReplyToMe(*message) {
			logger.Debug("counting group request in stats", "chat_type", message.Chat.Type)
			b.stats.GroupRequest()
		}
	case t.ChatTypePrivate:
		logger.Debug("counting private request in stats", "chat_type", message.Chat.Type)
		b.stats.PrivateRequest()
	}

	return ctx.Next(update)
}

func (b *Bot) chatHistory(ctx *th.Context, update t.Update) error {
	message := update.Message
	logger := b.handlerLogger(ctx)

	if message == nil {
		logger.Debug("history middleware skipped update without message")

		return ctx.Next(update)
	}

	logger.Debug("saving message to history", "chat_id", message.Chat.ID, "has_image", len(message.Photo) > 0)

	// Process message and store in context
	msgData := b.tgUserMessageToMessageData(*message, false)
	ctx = ctx.WithValue(requestContextMessageDataKey, msgData)

	// Save to history
	b.saveChatMessageToHistory(msgData)

	return ctx.Next(update)
}
