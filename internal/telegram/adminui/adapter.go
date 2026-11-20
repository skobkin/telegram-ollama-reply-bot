// Package adminui is the Telegram presentation adapter for the admin UI: it
// is the only admin-UI package that touches telego types, translating
// controller outcomes into Telegram IO (screen edits, ForceReply prompts,
// documents, chat-picker keyboards) and Telegram updates into controller
// inputs. Business logic lives in internal/adminui; domain operations in
// internal/adminconfig.
package adminui

import (
	"context"
	"log/slog"
	"strconv"
	"strings"
	"sync"

	t "github.com/mymmrac/telego"
	th "github.com/mymmrac/telego/telegohandler"
	tu "github.com/mymmrac/telego/telegoutil"

	core "telegram-ollama-reply-bot/internal/adminui"
	"telegram-ollama-reply-bot/internal/support/safeerr"
)

// msgAdminOnly mirrors the /stats denial text for non-admin /cancel and
// non-admin /admin reaching the adapter under a permissive access policy.
const msgAdminOnly = "This command is available only to administrators."

// Adapter performs all Telegram IO of the admin UI. Callback
// acknowledgements are exactly-once by construction: every handler path
// funnels through a single guarded answer() closure.
type Adapter struct {
	controller *core.Controller
	manager    *core.Manager
	adminIDs   []int64
	logger     *slog.Logger

	api       *t.Bot
	botUserID int64

	menuMu     sync.Mutex
	menuFailed map[int64]bool // admins whose per-chat command menu sync failed

	// wake hands chat IDs to the menu re-creation worker; nil until
	// StartWatching runs (and best-effort afterwards). Reopens use it when
	// their own ingress event can no longer cover the reopened menu — see
	// reopenDashboard.
	wakeMu sync.Mutex
	wake   func(int64)
}

// NewAdapter wires the adapter. The API client is bound at construction, not
// at route registration: the menu re-creation worker starts before the bot's
// routes exist (the dispatcher is watched from startup on), so a reminder
// delivered to an admin in that startup window reaches the worker's gate
// while RegisterRoutes has not run yet — a late bind would both blind the
// gate and race the write against the worker's reads. The bot user ID is
// still bound at RegisterRoutes time.
func NewAdapter(controller *core.Controller, manager *core.Manager, adminIDs []int64, logger *slog.Logger, api *t.Bot) *Adapter {
	return &Adapter{
		controller: controller,
		manager:    manager,
		adminIDs:   adminIDs,
		logger:     logger,
		api:        api,
		menuFailed: make(map[int64]bool),
	}
}

// RegisterRoutes registers the admin UI routes and binds the bot user ID. It
// must be called after the generic gates (logging, DM restrictions, access
// policy) so admin updates terminate before the conversation pipeline
// (history, stats, LLM).
//
// Route order inside is load-bearing: the /admin + /cancel command route
// comes first so replying to a prompt with /admin reopens the panel instead
// of being consumed as input; the input-consumption route precedes the
// conversation pipeline so correlated replies never reach the LLM.
func (a *Adapter) RegisterRoutes(bh *th.BotHandler, botUserID int64, commandForMe th.Predicate) {
	a.botUserID = botUserID

	adminCommand := th.And(
		commandForMe,
		th.Or(th.CommandEqual("admin"), th.CommandEqual("cancel")),
		a.privateChat,
	)
	bh.HandleMessage(a.handleAdminCommand, adminCommand)

	bh.HandleCallbackQuery(a.handleCallback, th.CallbackDataPrefix(core.CallbackPrefix))

	bh.HandleMessage(a.handleAdminInput, th.And(a.privateChat, a.fromAdmin, a.replyToBot))

	bh.HandleMessage(a.handleChatShared, th.And(a.privateChat, a.fromAdmin, a.hasChatShared))
}

// Predicates (telegohandler has no private-chat one; exactness matters here —
// admin IO must never run in groups).

func (a *Adapter) privateChat(_ context.Context, update t.Update) bool {
	return update.Message != nil && update.Message.Chat.Type == t.ChatTypePrivate
}

func (a *Adapter) fromAdmin(_ context.Context, update t.Update) bool {
	return update.Message != nil && update.Message.From != nil && a.controller.IsAdmin(update.Message.From.ID)
}

func (a *Adapter) replyToBot(_ context.Context, update t.Update) bool {
	message := update.Message
	toBot := message != nil && message.ReplyToMessage != nil &&
		message.ReplyToMessage.From != nil && message.ReplyToMessage.From.ID == a.botUserID

	return toBot
}

func (a *Adapter) hasChatShared(_ context.Context, update t.Update) bool {
	return update.Message != nil && update.Message.ChatShared != nil
}

// handleAdminCommand serves /admin and /cancel in private chats. With an
// empty BOT__ADMIN_IDS the narrow bootstrap exception in accessGate lets a
// private /admin through; the answer is the setup diagnostic — no admin
// functionality exists without admins.
func (a *Adapter) handleAdminCommand(ctx *th.Context, message t.Message) error {
	adminID := message.From.ID
	command := commandName(message.Text)

	if len(a.adminIDs) == 0 {
		a.sendPlain(ctx, adminID, AdminControlsDisabledText(message.From))

		return nil
	}
	if !a.controller.IsAdmin(adminID) {
		a.sendPlain(ctx, adminID, msgAdminOnly)

		return nil
	}

	var (
		outcome core.Outcome
		err     error
	)
	if strings.EqualFold(command, "cancel") {
		outcome, err = a.cancelCommand(ctx, adminID)
	} else {
		outcome, err = a.reopenDashboard(ctx, adminID)
		if err == nil {
			a.retryAdminMenu(ctx, adminID)
		}
	}
	if err != nil {
		// Match the callback/reply feedback convention so a failed /admin
		// is never a silent no-op for the admin.
		a.logger.Error("admin ui command failed", "admin_id", adminID, "command", command, "error", err)
		a.sendPlain(ctx, adminID, "Failed: "+err.Error())

		return nil //nolint:nilerr // handled: feedback sent to the admin
	}

	a.applyOutcome(ctx, adminID, outcome)

	return nil
}

// preCommandSessionState snapshots whether the admin's current session is
// live and carries a rendered menu, before a command replaces or reopens
// over it.
func (a *Adapter) preCommandSessionState(adminID int64) (wasLive, hadMenu bool) {
	if session := a.manager.Session(adminID); session != nil {
		_, _, msgID, _ := session.DetachedSnapshot()
		hadMenu = msgID != 0
		_, wasLive = a.manager.ActiveSession(adminID)
	}

	return wasLive, hadMenu
}

// reopenDashboard opens the dashboard, re-anchoring the quiet window when the
// reopen cannot be covered by its own ingress event. The /admin ingress event
// publishes before the handler runs — and while the handler is delayed behind
// the route middlewares, the debounce can fire against the session the command
// is about to replace: a dead (expired or detached) session makes the core
// silently skip, consuming the only activity notification, and the reopen then
// just edits the buried menu in place because admin-UI sends publish no events
// of their own. A dead previous session that carried a rendered menu is exactly
// that unrecoverable shape, so the successful reopen itself wakes the worker.
// A live previous session needs no wake — its event lands on a live session and
// re-creates normally — and no previous menu means a fresh menu was just sent
// to the bottom of the chat.
func (a *Adapter) reopenDashboard(ctx context.Context, adminID int64) (core.Outcome, error) {
	wasLive, hadMenu := a.preCommandSessionState(adminID)

	outcome, err := a.controller.OpenDashboard(ctx, adminID)
	if err == nil && hadMenu && !wasLive {
		a.notifyRecreate(adminID)
	}

	return outcome, err
}

// cancelCommand cancels any pending input, re-anchoring the quiet window when
// the cancellation falls back to reopening the dashboard over a dead session
// — the same unrecoverable shape as the delayed /admin reopen: with the
// session expired, the command's ingress event fires against a session the
// core silently skips (Cancel's own Apply reports the session expired and
// falls back), and the fallback edits the buried menu in place because
// admin-UI sends publish no events of their own. The core marks the fallback
// on the outcome (Outcome.Reopened). A live session needs no wake: cancelling
// its input re-renders the session's own message, and the event path's
// pending re-arm loop covers a fire that landed before the handler.
func (a *Adapter) cancelCommand(ctx context.Context, adminID int64) (core.Outcome, error) {
	wasLive, hadMenu := a.preCommandSessionState(adminID)

	outcome, err := a.controller.Cancel(ctx, adminID)
	if err == nil && outcome.Reopened && hadMenu && !wasLive {
		a.notifyRecreate(adminID)
	}

	return outcome, err
}

// notifyRecreate hands one chat ID to the re-creation worker. Non-blocking:
// a busy or stopped worker drops it, matching the event path's best-effort
// contract. No-op before StartWatching.
func (a *Adapter) notifyRecreate(chatID int64) {
	a.wakeMu.Lock()
	wake := a.wake
	a.wakeMu.Unlock()
	if wake != nil {
		wake(chatID)
	}
}

// handleCallback acknowledges exactly once and pre-answers: fast validation
// (no DB, no session lock) answers the toast paths; otherwise the spinner is
// cleared with an empty answer BEFORE any processing, and outcome feedback is
// rendered on the screen instead of a second answer.
func (a *Adapter) handleCallback(ctx *th.Context, query t.CallbackQuery) error {
	var answered bool
	answer := func(text string) {
		if answered {
			return
		}
		answered = true

		params := tu.CallbackQuery(query.ID)
		if text != "" {
			params.Text = text
		}
		if err := ctx.Bot().AnswerCallbackQuery(ctx, params); err != nil {
			a.logger.Warn("admin ui cannot answer callback query", "admin_id", query.From.ID,
				"error", safeerr.WrapTransport(errAdminCallbackAck, err))
		}
	}

	if !decodesAsAction(query.Data) {
		answer(core.AnswerUnknown)

		return nil
	}
	if !a.controller.IsAdmin(query.From.ID) {
		answer(core.AnswerDenied)

		return nil
	}

	msgID, ok := callbackOrigin(query)
	if !ok {
		answer(core.AnswerMenuGone)

		return nil
	}
	session, live := a.manager.ActiveSession(query.From.ID)
	if !live {
		answer(core.AnswerMenuGone)

		return nil
	}
	if _, _, sessionMsgID, _ := session.DetachedSnapshot(); sessionMsgID != 0 && sessionMsgID != msgID {
		answer(core.AnswerMenuGone)

		return nil
	}

	answer(core.AnswerSilent)

	outcome, err := a.controller.HandleCallback(ctx, query.From.ID, msgID, query.Data)
	if err != nil {
		a.logger.Error("admin ui callback failed", "admin_id", query.From.ID, "error", err)

		return nil
	}

	a.applyOutcome(ctx, query.From.ID, outcome)

	return nil
}

// decodesAsAction reports whether the callback data parses as an admin UI
// action (prefix, known verb, sequence). Malformed data is expected hostile
// input, answered with a toast — not an operational error.
func decodesAsAction(data string) bool {
	_, err := core.DecodeAction(data)

	return err == nil
}

// callbackOrigin validates that the callback arrived on an accessible message
// in the pressing user's own private chat, returning its message ID.
func callbackOrigin(query t.CallbackQuery) (int, bool) {
	origin := query.Message
	if origin == nil || !origin.IsAccessible() {
		return 0, false
	}

	chat := origin.GetChat()
	if chat.Type != t.ChatTypePrivate || chat.ID != query.From.ID {
		return 0, false
	}

	return origin.GetMessageID(), true
}

// applyOutcome performs the effects of one controller call in a fixed order:
// the committed render first (through the per-admin render gate), then
// cleanup, then input requests, documents, and chat verification.
func (a *Adapter) applyOutcome(ctx *th.Context, adminID int64, outcome core.Outcome) {
	if outcome.Render != nil && outcome.Session != nil {
		a.applyRender(ctx, adminID, outcome.Render, outcome.Session)
	}

	for _, msgID := range outcome.DeleteMsgIDs {
		if err := a.api.DeleteMessage(ctx, &t.DeleteMessageParams{ChatID: tu.ID(adminID), MessageID: msgID}); err != nil {
			a.logger.Debug("admin ui message cleanup failed", "admin_id", adminID, "message_id", msgID,
				"error", safeerr.WrapTransport(errAdminMessageCleanup, err))
		}
	}

	// Keyboard removal precedes verification and any other outcome IO: the
	// keyboard must be dismissed before GetChat verification runs, so the
	// selection cannot be confirmed twice.
	if outcome.DropPicker != "" {
		a.removePickerKeyboard(ctx, adminID, outcome.DropPicker)
	}

	if outcome.Message != nil {
		a.sendPlain(ctx, adminID, outcome.Message.Text)
	}
	if outcome.Input != nil {
		a.sendInputPrompt(ctx, outcome.Session, adminID, outcome.Input)
	}
	if outcome.Picker != nil {
		a.sendPickerKeyboard(ctx, outcome.Session, adminID, outcome.Picker)
	}
	// The photo precedes the document (inline chart first, canonical JSON
	// second); their failure modes are independent and both are attempted
	// unconditionally.
	if outcome.Photo != nil {
		a.sendPhotoPayload(ctx, adminID, outcome.Photo)
	}
	if outcome.Document != nil {
		a.sendDocumentPayload(ctx, adminID, outcome.Document)
	}
	if outcome.Verify != nil {
		a.verifyChat(ctx, adminID, outcome.Verify)
	}
}

// commandName extracts the bare command name from "/admin@bot args".
func commandName(text string) string {
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return ""
	}

	name := strings.TrimPrefix(fields[0], "/")
	if at := strings.IndexByte(name, '@'); at >= 0 {
		name = name[:at]
	}

	return name
}

// AdminControlsDisabledText is the bootstrap diagnostic shown to a private
// /admin sender when BOT__ADMIN_IDS is empty (the only update the narrow
// access-gate exception lets through).
func AdminControlsDisabledText(user *t.User) string {
	text := "Admin controls are disabled: BOT__ADMIN_IDS is empty."
	if user == nil {
		return text
	}

	userID := strconv.FormatInt(user.ID, 10)

	return text + "\nYour Telegram user ID: " + userID + "\nSet BOT__ADMIN_IDS=" + userID
}
