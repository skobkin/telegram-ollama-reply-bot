package adminui

import (
	"strings"

	t "github.com/mymmrac/telego"
	th "github.com/mymmrac/telego/telegohandler"
	tu "github.com/mymmrac/telego/telegoutil"

	core "telegram-ollama-reply-bot/internal/adminui"
)

// applyRender performs one visible menu update through the per-admin render
// gate: it edits the known menu message, and resends a fresh one when the old
// message is gone (adopting the new message ID).
func (a *Adapter) applyRender(ctx *th.Context, adminID int64, render *core.Render, session *core.Session) {
	err := a.manager.ApplyRender(render, session, func() error {
		if render.MsgID != 0 {
			params := &t.EditMessageTextParams{
				ChatID:      tu.ID(adminID),
				MessageID:   render.MsgID,
				Text:        render.Built.Text,
				ReplyMarkup: inlineKeyboard(render.Built),
			}
			_, err := a.api.EditMessageText(ctx, params)
			switch {
			case err == nil:
				// Adopt the edited message: after a session replacement the
				// fresh session starts with MsgID 0 even though it just
				// edited the live menu — without adopting, the next render
				// would send a duplicate menu instead of editing. Adoption is
				// scoped to the rendering session: if /admin replaced it
				// while the edit was in flight, the stamp is refused rather
				// than redirecting the replacement's menu.
				a.manager.AdoptMsgID(session, render.MsgID)

				return nil
			case isMessageGone(err):
				a.logger.Debug("admin ui menu message gone, resending", "admin_id", adminID, "message_id", render.MsgID)
			default:
				return err
			}
		}

		params := tu.Message(tu.ID(adminID), render.Built.Text)
		params.ReplyMarkup = inlineKeyboard(render.Built)
		message, err := a.api.SendMessage(ctx, params)
		if err != nil {
			return err
		}
		a.manager.AdoptMsgID(session, message.MessageID)

		return nil
	})
	if err != nil {
		a.logger.Warn("admin ui render failed", "admin_id", adminID, "error", err)
	}
}

// inlineKeyboard converts a screen button grid into an inline keyboard; nil
// when the screen has no buttons. All screens render as plain text — no parse
// mode: titles, usernames, and values are third-party controlled.
func inlineKeyboard(screen *core.Screen) *t.InlineKeyboardMarkup {
	if len(screen.Rows) == 0 {
		return nil
	}

	rows := make([][]t.InlineKeyboardButton, 0, len(screen.Rows))
	for _, row := range screen.Rows {
		buttons := make([]t.InlineKeyboardButton, 0, len(row))
		for _, button := range row {
			buttons = append(buttons, t.InlineKeyboardButton{
				Text:         button.Label,
				CallbackData: button.Data,
				Style:        button.Style,
			})
		}
		rows = append(rows, buttons)
	}

	return &t.InlineKeyboardMarkup{InlineKeyboard: rows}
}

// sendInputPrompt sends a correlated ForceReply prompt and records its message
// ID as the very next statement after the API call — a pending input is never
// matchable before its prompt message ID is known. The stamp is scoped to the
// committing session: a send completing after /admin replaced the session is
// dropped instead of correlating the replacement session's pending.
func (a *Adapter) sendInputPrompt(ctx *th.Context, session *core.Session, adminID int64, request *core.InputRequest) {
	params := tu.Message(tu.ID(adminID), request.Text)
	params.ReplyMarkup = &t.ForceReply{
		ForceReply:            true,
		InputFieldPlaceholder: request.Placeholder,
	}

	message, err := a.api.SendMessage(ctx, params)
	if err != nil {
		a.logger.Warn("admin ui input prompt failed", "admin_id", adminID, "error", err)

		return
	}
	if err := a.manager.SetPendingPromptMsgID(session, request.PendingID, message.MessageID); err != nil {
		a.logger.Debug("admin ui input prompt not correlated", "admin_id", adminID, "error", err)
	}
}

// sendPlain sends a plain text message without any keyboard.
func (a *Adapter) sendPlain(ctx *th.Context, adminID int64, text string) {
	if _, err := a.api.SendMessage(ctx, tu.Message(tu.ID(adminID), text)); err != nil {
		a.logger.Warn("admin ui message send failed", "admin_id", adminID, "error", err)
	}
}

// sendDocumentPayload delivers a screen payload as a document (large prompt
// bodies, downloads).
func (a *Adapter) sendDocumentPayload(ctx *th.Context, adminID int64, payload *core.DocumentPayload) {
	params := tu.Document(tu.ID(adminID), tu.FileFromBytes(payload.Content, payload.Filename))
	if _, err := a.api.SendDocument(ctx, params); err != nil {
		a.logger.Warn("admin ui document send failed", "admin_id", adminID, "error", err)
	}
}

// isMessageGone reports whether the edit failed because the target message
// no longer exists (deleted or too old to edit) — the resend signal.
func isMessageGone(err error) bool {
	if err == nil {
		return false
	}

	description := strings.ToLower(err.Error())

	return strings.Contains(description, "not found") ||
		strings.Contains(description, "message_id_invalid")
}
