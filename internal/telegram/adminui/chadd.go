package adminui

import (
	"strings"

	t "github.com/mymmrac/telego"
	th "github.com/mymmrac/telego/telegohandler"
	tu "github.com/mymmrac/telego/telegoutil"

	"telegram-ollama-reply-bot/internal/adminconfig"
	core "telegram-ollama-reply-bot/internal/adminui"
)

// sendPickerKeyboard sends the native chat picker: a reply keyboard with one
// request-chat button. The request is deliberately unconstrained — BotIsMember
// stays nil so chats the bot has never joined are selectable too; channels are
// excluded via ChatIsChannel=false. The keyboard is removed before the first
// shared chat is processed (handleChatShared).
func (a *Adapter) sendPickerKeyboard(ctx *th.Context, session *core.Session, adminID int64, picker *core.PickerRequest) {
	button := tu.KeyboardButton("Choose a chat")
	// The controller generates request IDs within the positive int32 range;
	// masking here keeps the boundary locally provable.
	requestID := picker.RequestID & 0x7fffffff
	button.RequestChat = &t.KeyboardButtonRequestChat{
		RequestID:     int32(requestID),
		ChatIsChannel: false,
	}

	params := tu.Message(tu.ID(adminID), picker.Text)
	params.ReplyMarkup = tu.Keyboard(tu.KeyboardRow(button))

	message, err := a.api.SendMessage(ctx, params)
	if err != nil {
		a.logger.Warn("admin ui picker keyboard failed", "admin_id", adminID, "error", err)

		return
	}
	// The picker prompt doubles as the pending-input anchor: the ChatShared
	// response references its message, so correlate it like a ForceReply —
	// scoped to the committing session exactly like sendInputPrompt.
	if err := a.manager.SetPendingPromptMsgID(session, picker.PendingID, message.MessageID); err != nil {
		a.logger.Debug("admin ui picker prompt not correlated", "admin_id", adminID, "error", err)
	}
}

// removePickerKeyboard removes the reply keyboard right after a chat was
// selected; sendMessage requires non-empty text, so it rides on a short
// acknowledgement line.
func (a *Adapter) removePickerKeyboard(ctx *th.Context, adminID int64) {
	params := tu.Message(tu.ID(adminID), "Chat selection received.")
	params.ReplyMarkup = tu.ReplyKeyboardRemove()

	if _, err := a.api.SendMessage(ctx, params); err != nil {
		a.logger.Warn("admin ui keyboard removal failed", "admin_id", adminID, "error", err)
	}
}

// handleChatShared consumes EVERY chat-selection response in an admin DM —
// unmatched, expired, cancelled, or duplicated selections never fall through
// to the conversation pipeline. The reply keyboard disappears before the
// first event is processed.
func (a *Adapter) handleChatShared(ctx *th.Context, message t.Message) error {
	adminID := message.From.ID
	shared := message.ChatShared

	a.removePickerKeyboard(ctx, adminID)

	outcome, err := a.controller.HandleChatShared(ctx, adminID, core.ChatSharedInput{
		RequestID: shared.RequestID,
		ChatID:    shared.ChatID,
		Title:     shared.Title,
		Username:  shared.Username,
	})
	if err != nil {
		a.logger.Error("admin ui chat shared failed", "admin_id", adminID, "error", err)

		return nil
	}

	a.applyOutcome(ctx, adminID, outcome)

	return nil
}

// verifyChat resolves the chat via GetChat and completes the add with real
// display data; on failure the completion proceeds with a minimal
// unverified entry instead.
func (a *Adapter) verifyChat(ctx *th.Context, adminID int64, verify *core.VerifyChat) {
	var (
		entry    adminconfig.ChatCatalogEntry
		verified bool
	)

	info, err := a.api.GetChat(ctx, &t.GetChatParams{ChatID: tu.ID(verify.ChatID)})
	switch {
	case err != nil:
		a.logger.Warn("admin ui cannot verify chat, adding unverified",
			"admin_id", adminID, "chat_id", verify.ChatID, "error", err)
		entry = adminconfig.ChatCatalogEntry{ChatID: verify.ChatID, ChatType: "unknown"}
	case info == nil:
		a.logger.Warn("admin ui empty chat info, adding unverified", "admin_id", adminID, "chat_id", verify.ChatID)
		entry = adminconfig.ChatCatalogEntry{ChatID: verify.ChatID, ChatType: "unknown"}
	default:
		entry = catalogEntryFromChatInfo(info)
		verified = true
	}

	outcome, err := a.controller.CompleteChatAdd(ctx, adminID, entry, verified)
	if err != nil {
		a.logger.Error("admin ui chat add failed", "admin_id", adminID, "chat_id", verify.ChatID, "error", err)

		return
	}

	a.applyOutcome(ctx, adminID, outcome)
}

// catalogEntryFromChatInfo maps a GetChat result to a catalog entry. First
// and last name apply to private chats; title to groups.
func catalogEntryFromChatInfo(info *t.ChatFullInfo) adminconfig.ChatCatalogEntry {
	return adminconfig.ChatCatalogEntry{
		ChatID:      info.ID,
		ChatType:    info.Type,
		Title:       info.Title,
		Username:    info.Username,
		DisplayName: strings.TrimSpace(strings.Join([]string{info.FirstName, info.LastName}, " ")),
	}
}
