package bot

import (
	"strings"
	"time"

	"telegram-ollama-reply-bot/internal/adminconfig"
	"telegram-ollama-reply-bot/internal/logging"

	t "github.com/mymmrac/telego"
	th "github.com/mymmrac/telego/telegohandler"
)

// requestContextMessageDataKey is the context key for storing processed message data in request context
const requestContextMessageDataKey = "message_data"

// dmAdminsOnlyGate enforces BOT__DM_ADMINS_ONLY: when enabled, private-chat
// messages from non-admin users are dropped before any catalog, whitelist,
// history, stats, command, or LLM side effect. Admins are identified solely by
// the BOT__ADMIN_IDS trust root, so an empty admin list drops every private
// message. The drop is silent: a reply would itself be a side effect and an
// abuse vector for arbitrary DM users. Group chats are never affected.
func (b *Bot) dmAdminsOnlyGate(ctx *th.Context, update t.Update) error {
	message := update.Message
	if message == nil || !b.cfg.DMAdminsOnly || message.Chat.Type != t.ChatTypePrivate || b.isFromAdmin(message) {
		return ctx.Next(update)
	}

	logger := b.handlerLogger(ctx).With("chat_id", message.Chat.ID)
	if message.From != nil {
		logger = logger.With("from_id", message.From.ID)
	}
	logger.Info("dropping non-admin private message (BOT__DM_ADMINS_ONLY)")

	return nil
}

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

func (b *Bot) collectChatCatalog(ctx *th.Context, update t.Update) error {
	message := update.Message
	if message == nil || b.admin == nil {
		return ctx.Next(update)
	}

	entry := adminconfig.ChatCatalogEntry{
		ChatID:      message.Chat.ID,
		ChatType:    message.Chat.Type,
		Title:       message.Chat.Title,
		Username:    message.Chat.Username,
		DisplayName: chatDisplayName(message.Chat),
		FirstSeenAt: time.Now().UTC(),
		LastSeenAt:  time.Now().UTC(),
	}
	if err := b.admin.Store().UpsertChatCatalog(ctx.Context(), entry); err != nil {
		b.handlerLogger(ctx).Warn("failed to update chat catalog", "chat_id", message.Chat.ID, "error", err)
	}

	return ctx.Next(update)
}

// unknownCommandHandler drops commands outside the supported user-facing
// set (including removed legacy admin commands) before the conversation
// pipeline: they never reach history, statistics, or the LLM. The drop is
// silent by operator decision — a reply would be an abuse vector.
func (b *Bot) unknownCommandHandler(ctx *th.Context, message t.Message) error {
	command := "unknown"
	if matches := th.CommandRegexp.FindStringSubmatch(message.Text); len(matches) == th.CommandMatchGroupsLen {
		command = "/" + matches[th.CommandMatchCmdGroup]
	}

	b.handlerLogger(ctx).Debug("dropping unknown command", "command", command)

	return nil
}

func (b *Bot) accessGate(ctx *th.Context, update t.Update) error {
	message := update.Message
	if message == nil || b.admin == nil {
		return ctx.Next(update)
	}

	if b.isBootstrapAdminCommand(message) {
		return ctx.Next(update)
	}

	allowed, err := b.admin.ShouldAllowChat(ctx.Context(), message.Chat.ID, b.isAdminDM(message))
	if err != nil {
		return err
	}
	if allowed {
		return ctx.Next(update)
	}

	b.handlerLogger(ctx).Info("ignoring non-whitelisted chat", "chat_id", message.Chat.ID)

	return nil
}

// isBootstrapAdminCommand is the narrow empty-admin-list bootstrap exception:
// with BOT__ADMIN_IDS empty and BOT__DM_ADMINS_ONLY off, a private /admin
// command is admitted so the admin UI can answer with the setup diagnostic
// (how to set BOT__ADMIN_IDS, including the sender's ID). Nothing else
// qualifies, no administrative functionality exists without admins, and the
// earlier dmAdminsOnlyGate still drops every private message when the
// restriction is enabled — the exception cannot bypass it.
func (b *Bot) isBootstrapAdminCommand(message *t.Message) bool {
	if len(b.cfg.AdminIDs) > 0 || b.cfg.DMAdminsOnly || message.Chat.Type != t.ChatTypePrivate {
		return false
	}

	matches := th.CommandRegexp.FindStringSubmatch(message.Text)
	if len(matches) != th.CommandMatchGroupsLen {
		return false
	}

	return strings.EqualFold(matches[th.CommandMatchCmdGroup], "admin")
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
