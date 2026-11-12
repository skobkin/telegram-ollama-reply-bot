package adminui

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"telegram-ollama-reply-bot/internal/adminconfig"
)

// AdminInputMarker prefixes every admin input request message the adapter
// sends; correlated replies are matched by it plus the pending state.
const AdminInputMarker = "⚙️ Admin input"

// PromptBodyLimit is the hard cap on prompt bodies accepted from uploads and
// text input (32 KiB, enforced on actual bytes — the adapter additionally
// bounds the document download itself).
const PromptBodyLimit = 32 * 1024

// Answer texts for callback acknowledgements (exact-once answering is the
// adapter's job; the controller only decides the text).
const (
	AnswerSilent   = ""
	AnswerUnknown  = "Unknown action"
	AnswerDenied   = "Not authorized"
	AnswerMenuGone = "This menu is no longer active — open /admin"
)

// Feedback messages for consumed admin inputs (never reach the LLM).
const (
	msgInputInactive      = "This input request is no longer active. Open /admin to continue."
	msgUnmatchedReply     = "This reply could not be matched to an active admin input and was not processed."
	msgChatSharedInactive = "This chat selection is no longer active. Open /admin to continue."
	msgChatSharedDone     = "This chat selection was already processed."
	// Chat-picker keyboard removal acknowledgements: the removal rides on a
	// short message (sendMessage requires non-empty text), so its text doubles
	// as the feedback line. Exported: the adapter needs the closure line for
	// a picker send whose pending died in flight.
	MsgPickerReceived = "Chat selection received."
	MsgPickerClosed   = "Chat selection closed."
	noticeMenuUpdated = "Menu updated."
)

var (
	// ErrMsgMismatch means the callback arrived on a message that is not the
	// session's active menu message.
	ErrMsgMismatch = errors.New("callback message does not match the active menu")
	// errInvalidArg means the callback decoded but carried unusable
	// arguments; it maps to an "Unknown action" toast without any writes.
	errInvalidArg = errors.New("invalid callback argument")
)

// InputRequest asks the adapter to send a correlated ForceReply prompt. The
// message ID of the sent message must be recorded via
// Manager.SetPendingPromptMsgID immediately after the send, keyed to the
// committing session plus PendingID so a replaced pending — or a replaced
// session whose pendings reuse the same IDs — can never be stamped by a late
// send. Retry prompts after invalid input carry the SAME pending's ID.
type InputRequest struct {
	PendingID   uint64
	Text        string
	Placeholder string
}

// PickerRequest asks the adapter to send the chat-picker reply keyboard;
// PendingID keys its correlation exactly like InputRequest.
type PickerRequest struct {
	PendingID uint64
	RequestID int
	Text      string
}

// TextMessage asks the adapter to send a plain message (input feedback,
// small prompt views).
type TextMessage struct {
	Text string
}

// VerifyChat asks the adapter to verify a chat via GetChat and then call
// CompleteChatAdd with the result (nil-entry semantics are handled by the
// caller constructing a minimal entry).
type VerifyChat struct {
	ChatID int64
}

// Outcome is everything the adapter must perform after one controller call:
// the callback acknowledgement text (callbacks only), the committed render
// (apply through Manager.ApplyRender), and side-effect requests in any order
// after the acknowledgement and render.
type Outcome struct {
	Answer       string
	Render       *Render
	Session      *Session
	Input        *InputRequest
	Picker       *PickerRequest
	Document     *DocumentPayload
	Message      *TextMessage
	Verify       *VerifyChat
	DeleteMsgIDs []int
	// DropPicker removes the chat-picker reply keyboard, with the text as the
	// acknowledgement line. Empty keeps the keyboard: in particular, a stale
	// or mismatched chat selection must never dismiss a newer active picker.
	DropPicker string
}

// fx accumulates the side effects a verb handler requests; it is discarded
// when the handler fails.
type fx struct {
	input      *InputRequest
	picker     *PickerRequest
	document   *DocumentPayload
	message    *TextMessage
	verify     *VerifyChat
	deletes    []int
	dropPicker string
}

func (f fx) outcome() Outcome {
	return Outcome{
		Input:        f.input,
		Picker:       f.picker,
		Document:     f.document,
		Message:      f.message,
		Verify:       f.verify,
		DeleteMsgIDs: f.deletes,
		DropPicker:   f.dropPicker,
	}
}

// foldPickerRemoval turns an orphaned chat-picker keyboard marker into the
// removal effect. The removal must carry non-empty text, so a feedback line
// already queued for the outcome doubles as the acknowledgement; otherwise a
// neutral closure line is used. An explicitly set DropPicker (the acceptance
// acknowledgement) wins.
func foldPickerRemoval(outcome *Outcome, orphaned bool) {
	if !orphaned || outcome.DropPicker != "" {
		return
	}

	if outcome.Message != nil {
		outcome.DropPicker = outcome.Message.Text
		outcome.Message = nil

		return
	}

	outcome.DropPicker = MsgPickerClosed
}

// ReplyInput is one reply in an admin DM to a bot message, already
// classified by the adapter (marker present, or unclassifiable). The
// controller decides whether it matches a live pending input.
type ReplyInput struct {
	// ReplyToMsgID is the message being replied to (pending correlation).
	ReplyToMsgID int
	// TextAvailable reports whether the replied-to message text was
	// available for classification; when false the controller fails closed.
	TextAvailable bool
	// Text is the reply's text payload (may be empty for documents).
	Text string
	// Document is the reply's downloaded document payload, if any.
	Document *DocumentInput
	// ReplyMsgID is the reply's own message ID (cleanup target).
	ReplyMsgID int
}

// DocumentInput is a downloaded prompt document.
type DocumentInput struct {
	Filename string
	Content  []byte
}

// ChatSharedInput is a chat-selection response mapped from the Telegram
// ChatShared payload.
type ChatSharedInput struct {
	RequestID int
	ChatID    int64
	Title     string
	Username  string
}

// Controller orchestrates adminconfig operations into screens, session
// transitions, and adapter effects. It performs no IO itself: store access
// happens inside Manager.Apply commits, Telegram IO is expressed as Outcome
// effects. The package doc in callback.go describes the layering.
type Controller struct {
	manager  *Manager
	service  *adminconfig.Service
	adminIDs []int64
	stats    func() string
	started  time.Time
}

// NewController wires the controller. stats may be nil (the statistics screen
// then reports unavailability).
func NewController(manager *Manager, service *adminconfig.Service, adminIDs []int64, stats func() string) *Controller {
	return &Controller{
		manager:  manager,
		service:  service,
		adminIDs: adminIDs,
		stats:    stats,
		started:  manager.Now(),
	}
}

// IsAdmin reports whether the user may use the admin UI at all.
func (c *Controller) IsAdmin(userID int64) bool {
	for _, id := range c.adminIDs {
		if id == userID {
			return true
		}
	}

	return false
}

// OpenDashboard starts a fresh admin session (replacing any previous one) and
// returns its first render. Render.MsgID carries the previous session's
// message ID (0 when unknown) so the adapter can edit instead of sending.
// A live picker keyboard of the replaced session is orphaned by the
// replacement (its pending dies with the session), so the outcome removes it.
func (c *Controller) OpenDashboard(ctx context.Context, adminID int64) (Outcome, error) {
	oldMsgID := 0
	pickerLive := false
	if old := c.manager.Session(adminID); old != nil {
		_, _, oldMsgID, _ = old.DetachedSnapshot()
		pickerLive = old.LivePickerKeyboard()
	}

	session := c.manager.Replace(adminID, ScreenState{Kind: ScreenDashboard})

	// Snapshot under the session lock: the fresh session is already visible
	// to concurrent updates (a stale button press can commit a notice and
	// bump Seq/Epoch the moment Replace returns).
	seq, state, _, epoch := session.Snapshot()

	screen, err := c.buildScreen(ctx, seq, state)
	if err != nil {
		return Outcome{}, err
	}

	render := &Render{
		AdminID: adminID,
		Epoch:   epoch,
		Seq:     seq,
		MsgID:   oldMsgID,
		Screen:  state,
		Built:   screen,
	}

	outcome := Outcome{Session: session, Render: render}
	if pickerLive {
		outcome.DropPicker = MsgPickerClosed
	}

	return outcome, nil
}

// Cancel cancels any live pending input or confirmation; with nothing
// pending it reopens the dashboard (the /cancel chosen default).
func (c *Controller) Cancel(ctx context.Context, adminID int64) (Outcome, error) {
	if !c.IsAdmin(adminID) {
		return Outcome{}, nil
	}

	var effects fx
	hadPending := false
	result, err := c.manager.Apply(adminID, 0, func(s *Session) error {
		if s.Pending == nil && s.Confirm == nil {
			return nil
		}
		hadPending = true
		if s.Pending != nil && s.Pending.PromptMsgID != 0 {
			effects.deletes = append(effects.deletes, s.Pending.PromptMsgID)
		}
		s.dropPending()
		s.Confirm = nil
		s.Screen.Notice = "Cancelled."
		s.MarkRerender()

		return nil
	})
	if err != nil || !hadPending {
		return c.OpenDashboard(ctx, adminID)
	}

	outcome := effects.outcome()
	outcome.Message = &TextMessage{Text: "Cancelled."}
	c.attachRender(ctx, &outcome, result)

	return outcome, nil
}

// HandleCallback processes one admin UI callback. The returned Outcome.Answer
// is the single acknowledgement text; everything else happens in the UI.
func (c *Controller) HandleCallback(ctx context.Context, adminID int64, msgID int, data string) (Outcome, error) {
	if !c.IsAdmin(adminID) {
		return Outcome{Answer: AnswerDenied}, nil
	}

	action, err := DecodeAction(data)
	if err != nil {
		// A malformed callback is a handled outcome (toast), not a propagated
		// error — the caller already has its single acknowledgement.
		return Outcome{Answer: AnswerUnknown}, nil //nolint:nilerr
	}

	var effects fx
	result, err := c.manager.Apply(adminID, action.Seq, func(s *Session) error {
		if msgID != 0 && s.MsgID != 0 && msgID != s.MsgID {
			return ErrMsgMismatch
		}

		return c.handleVerb(ctx, adminID, s, action, &effects)
	})
	switch {
	case err == nil:
		outcome := effects.outcome()
		c.attachRender(ctx, &outcome, result)

		return outcome, nil
	case errors.Is(err, ErrSessionGone), errors.Is(err, ErrSessionExpired),
		errors.Is(err, ErrDetached), errors.Is(err, ErrMsgMismatch):
		return Outcome{Answer: AnswerMenuGone}, nil
	case errors.Is(err, ErrSeqStale):
		return c.notice(ctx, adminID, noticeMenuUpdated), nil
	case errors.Is(err, errInvalidArg):
		return Outcome{Answer: AnswerUnknown}, nil
	default:
		return c.notice(ctx, adminID, "Failed: "+err.Error()), nil
	}
}

// HandleReply processes a reply in an admin DM that the adapter classified
// as admin-input-related (marker present or unclassifiable). It never lets
// the reply reach ordinary conversation handling.
func (c *Controller) HandleReply(ctx context.Context, adminID int64, in ReplyInput) (Outcome, error) {
	if !c.IsAdmin(adminID) {
		return Outcome{}, nil
	}

	var effects fx
	result, err := c.manager.Apply(adminID, 0, func(s *Session) error {
		return c.processReply(ctx, adminID, s, in, &effects)
	})
	if err != nil {
		if errors.Is(err, ErrSessionGone) || errors.Is(err, ErrSessionExpired) || errors.Is(err, ErrDetached) {
			return Outcome{Message: &TextMessage{Text: msgInputInactive}}, nil
		}

		return Outcome{Message: &TextMessage{Text: "Failed: " + err.Error()}}, nil
	}

	outcome := effects.outcome()
	c.attachRender(ctx, &outcome, result)

	return outcome, nil
}

// HandleChatShared processes a chat-selection response in an admin DM. All
// such responses are consumed: a live matching pending triggers GetChat
// verification (Verify effect), everything else gets an inactive notice. The
// reply keyboard is removed only when the selection is accepted or its
// pending has expired — a stale or mismatched selection must not dismiss a
// newer, still-active picker.
func (c *Controller) HandleChatShared(_ context.Context, adminID int64, in ChatSharedInput) (Outcome, error) {
	if !c.IsAdmin(adminID) {
		return Outcome{}, nil
	}

	var effects fx
	result, err := c.manager.Apply(adminID, 0, func(s *Session) error {
		p := s.Pending
		if p != nil && p.Expired(c.manager.Now()) {
			// Expired pendings still consume the selection. dropPending
			// orphans a live picker keyboard, and foldPickerRemoval turns the
			// notice into that keyboard's removal acknowledgement; for any
			// other pending the notice stands alone.
			s.dropPending()
			effects.message = &TextMessage{Text: msgChatSharedInactive}

			return nil
		}

		switch {
		case p == nil, p.Kind != PendingChatPicker:
			effects.message = &TextMessage{Text: msgChatSharedInactive}
		case p.RequestID == 0:
			effects.message = &TextMessage{Text: msgChatSharedDone}
		case p.RequestID != in.RequestID:
			effects.message = &TextMessage{Text: msgChatSharedInactive}
		default:
			// Accepted: consume the request one-shot so duplicates fall into
			// the "already processed" branch above, and remove the reply
			// keyboard before the verification runs.
			s.orphanPickerKeyboard()
			p.RequestID = 0
			p.ScopeChatID = in.ChatID
			p.PromptMsgID = 0
			effects.dropPicker = MsgPickerReceived
			effects.verify = &VerifyChat{ChatID: in.ChatID}
		}

		return nil
	})
	if err != nil {
		if errors.Is(err, ErrSessionGone) || errors.Is(err, ErrSessionExpired) || errors.Is(err, ErrDetached) {
			return Outcome{Message: &TextMessage{Text: msgChatSharedInactive}}, nil
		}

		return Outcome{Message: &TextMessage{Text: "Failed: " + err.Error()}}, nil
	}

	outcome := effects.outcome()
	foldPickerRemoval(&outcome, result.PickerOrphaned)

	return outcome, nil
}

// CompleteChatAdd finishes an add-chat flow after GetChat verification:
// catalog registration, allowlist membership, and navigation to the new chat
// card. It fails closed (no store writes) unless the session still awaits
// this exact completion.
func (c *Controller) CompleteChatAdd(ctx context.Context, adminID int64, entry adminconfig.ChatCatalogEntry, verified bool) (Outcome, error) {
	if !c.IsAdmin(adminID) {
		return Outcome{}, nil
	}

	var effects fx
	result, err := c.manager.Apply(adminID, 0, func(s *Session) error {
		p := s.Pending
		if p == nil || p.Kind != PendingChatPicker || p.RequestID != 0 || p.ScopeChatID != entry.ChatID {
			effects.message = &TextMessage{Text: msgChatSharedInactive}

			return nil
		}

		if err := c.service.RegisterChat(ctx, entry); err != nil {
			return err
		}
		if err := c.service.SetAllowlist(ctx, entry.ChatID, true, adminID); err != nil {
			return err
		}

		s.dropPending() // the pending is consumed; dropPending keeps the discipline uniform
		s.PushScreen(ScreenState{Kind: ScreenChatCard, ChatID: entry.ChatID})
		if verified {
			s.Screen.Notice = "Added to allowlist."
		} else {
			s.Screen.Notice = "Added manually (unverified)."
		}
		s.MarkRerender()

		return nil
	})
	if err != nil {
		if errors.Is(err, ErrSessionGone) || errors.Is(err, ErrSessionExpired) || errors.Is(err, ErrDetached) {
			return Outcome{Message: &TextMessage{Text: msgChatSharedInactive}}, nil
		}

		return Outcome{Message: &TextMessage{Text: "Failed: " + err.Error()}}, nil
	}

	outcome := effects.outcome()
	c.attachRender(ctx, &outcome, result)

	return outcome, nil
}

// notice commits a notice-only re-render of the current screen (stale
// callbacks, operation failures).
func (c *Controller) notice(ctx context.Context, adminID int64, text string) Outcome {
	result, err := c.manager.Apply(adminID, 0, func(s *Session) error {
		s.Screen.Notice = text
		s.MarkRerender()

		return nil
	})
	if err != nil {
		return Outcome{Answer: AnswerMenuGone}
	}

	outcome := Outcome{Answer: AnswerSilent, Session: result.Session}
	c.attachRender(ctx, &outcome, result)

	return outcome
}

// attachRender builds the presentation screen for a committed render and
// attaches it to the outcome. The committing session is attached even
// without a render: the adapter's post-IO correlation (prompt stamps, menu
// adoption) is keyed to the session that committed, never to "whatever
// session is live now". Build failures leave the render unset: the state is
// committed but the visible message stays as-is (fail-safe).
func (c *Controller) attachRender(ctx context.Context, outcome *Outcome, result ApplyResult) {
	if result.Session != nil {
		outcome.Session = result.Session
	}
	foldPickerRemoval(outcome, result.PickerOrphaned)
	if result.Render == nil {
		return
	}

	screen, err := c.buildScreen(ctx, result.Render.Seq, result.Render.Screen)
	if err != nil {
		return
	}

	result.Render.Built = screen
	outcome.Render = result.Render
}

// handleVerb dispatches one decoded callback action inside the session lock.
// Handlers touch the store first and session state last (the Apply fn
// discipline) and signal screen changes via MarkRerender; pure-output verbs
// (prompt views/downloads) change no session state, keeping the visible
// keyboard's sequence valid.
func (c *Controller) handleVerb(ctx context.Context, adminID int64, s *Session, action Action, fx *fx) error {
	now := c.manager.Now()

	switch action.Verb {
	case VerbHome:
		if s.Screen.Kind == ScreenDashboard {
			return rerenderInPlace(s)
		}
		s.PushScreen(ScreenState{Kind: ScreenDashboard})
		s.MarkRerender()

		return nil

	case VerbCatalog:
		return c.handleCatalogVerb(ctx, s, action, fx, now)

	case VerbChat:
		if len(action.Args) != 1 {
			return errInvalidArg
		}
		chatID, err := strconv.ParseInt(action.Args[0], 10, 64)
		if err != nil {
			return errInvalidArg
		}
		s.PushScreen(ScreenState{Kind: ScreenChatCard, ChatID: chatID})
		s.MarkRerender()

		return nil

	case VerbGlobalSet:
		s.PushScreen(ScreenState{Kind: ScreenGlobalSet})
		s.MarkRerender()

		return nil

	case VerbChatSet:
		scope := scopeFromScreen(s)
		if scope == 0 {
			return errInvalidArg
		}
		s.PushScreen(ScreenState{Kind: ScreenChatSet, ChatID: scope})
		s.MarkRerender()

		return nil

	case VerbField:
		name, _, err := fieldFromArgs(s, action)
		if err != nil {
			return err
		}
		s.PushScreen(ScreenState{Kind: ScreenFieldEditor, Field: name, ChatID: scopeFromScreen(s)})
		s.MarkRerender()

		return nil

	case VerbValue:
		return c.handleValueVerb(ctx, adminID, s, action)

	case VerbString:
		name, spec, err := fieldFromArgs(s, action)
		if err != nil {
			return err
		}
		if spec.Kind != adminconfig.KindString {
			return errInvalidArg
		}
		current, err := c.currentFieldValue(ctx, scopeFromScreen(s), name)
		if err != nil {
			return err
		}
		s.orphanPickerKeyboard() // a new input abandons a live picker keyboard
		s.Pending = &Pending{
			ID:          s.NextPendingID(),
			Kind:        PendingStringInput,
			Field:       name,
			ScopeChatID: scopeFromScreen(s),
			ExpiresAt:   now.Add(PendingTTL),
		}
		fx.input = &InputRequest{
			PendingID:   s.Pending.ID,
			Text:        stringInputPrompt(spec, current),
			Placeholder: "New " + spec.Label,
		}

		return nil

	case VerbClear:
		scope := scopeFromScreen(s)
		if scope == 0 {
			return errInvalidArg
		}
		name, _, err := fieldFromArgs(s, action)
		if err != nil {
			return err
		}
		if err := c.service.ClearChatOverride(ctx, scope, name, adminID); err != nil {
			return err
		}
		s.Screen.Notice = "Override cleared — using the global value."
		s.MarkRerender()

		return nil

	case VerbPrompts:
		s.PushScreen(ScreenState{Kind: ScreenPrompts, ChatID: scopeFromScreen(s)})
		s.MarkRerender()

		return nil

	case VerbPromptFeat:
		feature, ok := featureFromArgs(action)
		if !ok {
			return errInvalidArg
		}
		s.PushScreen(ScreenState{Kind: ScreenPromptDetail, Feature: feature, ChatID: scopeFromScreen(s)})
		s.MarkRerender()

		return nil

	case VerbPromptView:
		feature, ok := featureFromArgs(action)
		if !ok {
			return errInvalidArg
		}
		body, _, err := c.service.ActivePrompt(ctx, feature, scopeFromScreen(s))
		if err != nil {
			return err
		}
		if len(body) > PromptViewDocumentThreshold {
			fx.document = promptDocument(feature, body)
		} else {
			fx.message = &TextMessage{Text: body}
		}

		return nil

	case VerbPromptDL:
		feature, ok := featureFromArgs(action)
		if !ok {
			return errInvalidArg
		}
		body, _, err := c.service.ActivePrompt(ctx, feature, scopeFromScreen(s))
		if err != nil {
			return err
		}
		fx.document = promptDocument(feature, body)

		return nil

	case VerbPromptBL:
		feature, ok := featureFromArgs(action)
		if !ok {
			return errInvalidArg
		}
		body, has := c.service.BuiltinPrompt(feature)
		if !has {
			s.Screen.Notice = "No builtin template registered."
			s.MarkRerender()

			return nil
		}
		fx.document = promptDocument(feature, body)

		return nil

	case VerbPromptText:
		feature, ok := featureFromArgs(action)
		if !ok {
			return errInvalidArg
		}
		s.orphanPickerKeyboard() // a new input abandons a live picker keyboard
		s.Pending = &Pending{
			ID:          s.NextPendingID(),
			Kind:        PendingPromptText,
			Feature:     feature,
			ScopeChatID: scopeFromScreen(s),
			ExpiresAt:   now.Add(PendingTTL),
		}
		fx.input = &InputRequest{PendingID: s.Pending.ID, Text: promptTextInputPrompt(feature, s.Screen.ChatID)}

		return nil

	case VerbPromptUp:
		feature, ok := featureFromArgs(action)
		if !ok {
			return errInvalidArg
		}
		s.orphanPickerKeyboard() // a new input abandons a live picker keyboard
		s.Pending = &Pending{
			ID:          s.NextPendingID(),
			Kind:        PendingDocumentUpload,
			Feature:     feature,
			ScopeChatID: scopeFromScreen(s),
			ExpiresAt:   now.Add(PendingTTL),
		}
		fx.input = &InputRequest{PendingID: s.Pending.ID, Text: promptUploadPrompt(feature, s.Screen.ChatID)}

		return nil

	case VerbPromptRstC:
		scope := scopeFromScreen(s)
		if scope == 0 {
			return errInvalidArg
		}
		feature, ok := featureFromArgs(action)
		if !ok {
			return errInvalidArg
		}
		s.PushScreen(confirmState(
			fmt.Sprintf("Reset the chat template for %s?\n\nThe chat-scoped template will be deleted; the chat falls back to the global template.", promptFeatureLabel(feature)),
			true, scope, feature))
		s.Confirm = &PendingConfirm{Kind: ConfirmChatPrompt, ChatID: scope, Feature: feature}
		s.MarkRerender()

		return nil

	case VerbPromptRstG:
		if scopeFromScreen(s) != 0 {
			return errInvalidArg
		}
		feature, ok := featureFromArgs(action)
		if !ok {
			return errInvalidArg
		}
		s.PushScreen(confirmState(
			fmt.Sprintf("Restore the builtin %s template?\n\nThe stored global template will be deleted; every chat using it sees the builtin.", promptFeatureLabel(feature)),
			true, 0, feature))
		s.Confirm = &PendingConfirm{Kind: ConfirmGlobalPrompt, Feature: feature}
		s.MarkRerender()

		return nil

	case VerbAccess:
		page := 1
		if len(action.Args) == 1 {
			parsed, err := strconv.Atoi(action.Args[0])
			if err != nil || parsed < 1 {
				return errInvalidArg
			}
			page = parsed
		} else if len(action.Args) > 1 {
			return errInvalidArg
		}
		if s.Screen.Kind == ScreenAccess {
			s.Screen.Page = page
			s.Screen.Notice = ""
		} else {
			s.PushScreen(ScreenState{Kind: ScreenAccess, Page: page})
		}
		s.MarkRerender()

		return nil

	case VerbAllowAdd:
		if len(action.Args) != 1 {
			return errInvalidArg
		}
		chatID, err := strconv.ParseInt(action.Args[0], 10, 64)
		if err != nil {
			return errInvalidArg
		}
		if err := c.service.SetAllowlist(ctx, chatID, true, adminID); err != nil {
			return err
		}
		s.Screen.Notice = "Added to allowlist."
		s.MarkRerender()

		return nil

	case VerbAllowRem:
		if len(action.Args) != 1 {
			return errInvalidArg
		}
		chatID, err := strconv.ParseInt(action.Args[0], 10, 64)
		if err != nil {
			return errInvalidArg
		}
		description := fmt.Sprintf("Remove chat %d from the allowlist?\n\nUnder the allowlist policy the chat will stop being served. The policy itself stays unchanged.", chatID)
		if entry, found := c.catalogEntry(ctx, chatID); found {
			description = fmt.Sprintf("Remove %s (%d) from the allowlist?\n\nUnder the allowlist policy the chat will stop being served. The policy itself stays unchanged.", truncateRunes(ChatEntryName(entry), 40), chatID)
		}
		s.PushScreen(confirmState(description, true, chatID, ""))
		s.Confirm = &PendingConfirm{Kind: ConfirmAllowlistDrop, ChatID: chatID}
		s.MarkRerender()

		return nil

	case VerbAccessPol:
		if len(action.Args) != 1 {
			return errInvalidArg
		}
		policy := adminconfig.AccessPolicy(action.Args[0])
		if !adminconfig.IsValidAccessPolicy(policy) {
			return errInvalidArg
		}
		current, err := c.service.AccessPolicy(ctx)
		if err != nil {
			return err
		}
		if policy == current {
			s.Screen.Notice = "Policy " + string(policy) + " is already active."
			s.MarkRerender()

			return nil
		}
		description, danger, err := c.policySwitchDescription(ctx, policy)
		if err != nil {
			return err
		}
		s.PushScreen(confirmState(description, danger, 0, ""))
		s.Confirm = &PendingConfirm{Kind: ConfirmPolicySwitch, Policy: policy}
		s.MarkRerender()

		return nil

	case VerbResetChat:
		scope := scopeFromScreen(s)
		if scope == 0 {
			return errInvalidArg
		}
		description := fmt.Sprintf("Reset ALL settings overrides for chat %d?\n\nThe chat will use global values for every field. Chat prompt templates are not affected.", scope)
		if entry, found := c.catalogEntry(ctx, scope); found {
			description = fmt.Sprintf("Reset ALL settings overrides for %s?\n\nThe chat will use global values for every field. Chat prompt templates are not affected.", truncateRunes(ChatEntryName(entry), 40))
		}
		s.PushScreen(confirmState(description, true, scope, ""))
		s.Confirm = &PendingConfirm{Kind: ConfirmChatOverrides, ChatID: scope}
		s.MarkRerender()

		return nil

	case VerbBack:
		previous := s.Screen
		s.PopScreen()
		s.Screen.Notice = ""
		if s.Screen == previous && previous.Notice == "" {
			return nil // byte-identical re-render: no API call, no seq bump
		}
		s.MarkRerender()

		return nil

	case VerbConfirmYes:
		return c.executeConfirm(ctx, adminID, s)

	case VerbConfirmNo:
		if s.Confirm == nil {
			return nil
		}
		s.Confirm = nil
		s.PopScreen()
		s.Screen.Notice = "Cancelled."
		s.MarkRerender()

		return nil

	case VerbStats:
		if s.Screen.Kind == ScreenStatistics {
			return rerenderInPlace(s)
		}
		s.PushScreen(ScreenState{Kind: ScreenStatistics})
		s.MarkRerender()

		return nil

	default:
		return errInvalidArg
	}
}

func (c *Controller) handleCatalogVerb(_ context.Context, s *Session, action Action, fx *fx, now time.Time) error {
	arg := ""
	if len(action.Args) > 0 {
		arg = action.Args[0]
	}

	switch arg {
	case "s":
		s.orphanPickerKeyboard() // a new input abandons a live picker keyboard
		s.Pending = &Pending{ID: s.NextPendingID(), Kind: PendingSearchInput, ExpiresAt: now.Add(PendingTTL)}
		fx.input = &InputRequest{
			PendingID:   s.Pending.ID,
			Text:        searchInputPrompt(),
			Placeholder: "Search query",
		}

		return nil

	case "a":
		requestID := randomRequestID()
		// No orphan on purpose: a NEW picker supersedes the reply keyboard in
		// place (the fresh keyboard send replaces the old one on the client).
		s.Pending = &Pending{
			ID:        s.NextPendingID(),
			Kind:      PendingChatPicker,
			RequestID: requestID,
			ExpiresAt: now.Add(PendingTTL),
		}
		fx.picker = &PickerRequest{PendingID: s.Pending.ID, RequestID: requestID, Text: pickerPromptText()}

		return nil

	default:
		if filter, ok := parseCatalogFilter(arg); ok {
			if s.Screen.Kind == ScreenCatalog {
				s.Screen.Filter = filter
				s.Screen.Page = 1
				s.Screen.Notice = ""
			} else {
				s.PushScreen(ScreenState{Kind: ScreenCatalog, Filter: filter, Page: 1})
			}
			s.MarkRerender()

			return nil
		}

		page, err := strconv.Atoi(arg)
		if err != nil || page < 1 {
			return errInvalidArg
		}
		if s.Screen.Kind == ScreenCatalog {
			s.Screen.Page = page
			s.Screen.Notice = ""
		} else {
			s.PushScreen(ScreenState{Kind: ScreenCatalog, Page: page})
		}
		s.MarkRerender()

		return nil
	}
}

func (c *Controller) handleValueVerb(ctx context.Context, adminID int64, s *Session, action Action) error {
	if len(action.Args) != 2 {
		return errInvalidArg
	}

	scope := scopeFromScreen(s)
	name, spec, err := fieldFromArgs(s, action)
	if err != nil {
		return err
	}

	clearOverride := false
	value := action.Args[1]
	switch spec.Kind {
	case adminconfig.KindEnum:
		// validated inside the service Set operation
	case adminconfig.KindBool:
		switch value {
		case "on":
			value = "true"
		case "off":
			value = "false"
		default:
			return errInvalidArg
		}
	case adminconfig.KindBoolTriState:
		switch value {
		case "on":
			value = "true"
		case "off":
			value = "false"
		case "inh":
			clearOverride = true
		default:
			return errInvalidArg
		}
	default:
		return errInvalidArg
	}

	switch {
	case scope == 0:
		err = c.service.SetGlobalSetting(ctx, name, value, adminID)
	case clearOverride:
		err = c.service.ClearChatOverride(ctx, scope, name, adminID)
	default:
		err = c.service.SetChatSetting(ctx, scope, name, value, adminID)
	}
	if err != nil {
		return err
	}

	s.Screen.Notice = "Saved."
	s.MarkRerender()

	return nil
}

func (c *Controller) executeConfirm(ctx context.Context, adminID int64, s *Session) error {
	confirm := s.Confirm
	if confirm == nil {
		s.Screen.Notice = "Nothing to confirm."
		s.MarkRerender()

		return nil
	}

	var notice string
	switch confirm.Kind {
	case ConfirmPolicySwitch:
		if err := c.service.SetAccessPolicy(ctx, confirm.Policy, adminID); err != nil {
			return err
		}
		notice = "Policy switched to " + string(confirm.Policy) + "."
	case ConfirmAllowlistDrop:
		if err := c.service.SetAllowlist(ctx, confirm.ChatID, false, adminID); err != nil {
			return err
		}
		notice = "Removed from allowlist."
	case ConfirmChatOverrides:
		if err := c.service.ClearAllChatOverrides(ctx, confirm.ChatID, adminID); err != nil {
			return err
		}
		notice = "All overrides cleared — using global values."
	case ConfirmChatPrompt:
		if err := c.service.ResetChatPrompt(ctx, confirm.Feature, confirm.ChatID); err != nil {
			return err
		}
		notice = "Chat template removed — using the global template."
	case ConfirmGlobalPrompt:
		if err := c.service.RestoreBuiltinGlobalPrompt(ctx, confirm.Feature); err != nil {
			return err
		}
		notice = "Builtin template restored."
	default:
		return errInvalidArg
	}

	s.Confirm = nil
	s.PopScreen()
	s.Screen.Notice = notice
	s.MarkRerender()

	return nil
}

// processReply runs inside the session lock: it matches the reply against
// the live pending input and either processes it or fails closed.
func (c *Controller) processReply(ctx context.Context, adminID int64, s *Session, in ReplyInput, fx *fx) error {
	now := c.manager.Now()
	p := s.Pending

	if p == nil || p.PromptMsgID == 0 || p.PromptMsgID != in.ReplyToMsgID {
		if p != nil && p.Expired(now) {
			s.dropPending()
		}
		fx.message = unmatchedFeedback(in)

		return nil
	}

	if p.Expired(now) {
		s.dropPending()
		s.Screen.Notice = "Input request expired."
		s.MarkRerender()
		fx.message = &TextMessage{Text: msgInputInactive}

		return nil
	}

	// The matched prompt message stays correlated until the outcome is
	// known: when persistence fails, the pending must keep matching its
	// original prompt so the admin can simply reply again. invalid()
	// releases the ID so its fresh retry prompt can re-arm the SAME
	// pending; success paths clear the pending outright.
	promptMsgID := p.PromptMsgID

	deletePromptAndReply := func() {
		fx.deletes = append(fx.deletes, promptMsgID, in.ReplyMsgID)
	}

	// invalid records the failed attempt, cleans up, and either requests a
	// fresh prompt or cancels after MaxInputAttempts tries.
	invalid := func(err error) error {
		p.Attempts++
		p.PromptMsgID = 0 // released: the fresh retry prompt re-arms this pending
		deletePromptAndReply()

		if p.Attempts >= MaxInputAttempts {
			s.dropPending()
			s.Screen.Notice = "Input cancelled after too many invalid attempts."
			s.MarkRerender()
			fx.message = &TextMessage{Text: fmt.Sprintf("Input cancelled after %d invalid attempts: %v", MaxInputAttempts, err)}

			return nil
		}

		p.ExpiresAt = now.Add(PendingTTL)
		text, placeholder := c.retryPrompt(ctx, p, err)
		fx.input = &InputRequest{PendingID: p.ID, Text: text, Placeholder: placeholder}

		return nil
	}

	textOnly := func() (string, error) {
		if in.Document != nil {
			return "", errors.New("send a text message, not a document")
		}

		return strings.TrimSpace(in.Text), nil
	}

	switch p.Kind {
	case PendingStringInput:
		if strings.TrimSpace(in.Text) == "" && in.Document == nil {
			return invalid(errors.New("the value must not be empty"))
		}
		text, err := textOnly()
		if err != nil {
			return invalid(err)
		}
		spec, err := fieldSpecFor(p.Field, p.ScopeChatID)
		if err != nil {
			return err
		}
		value, err := spec.ParseAndValidate(text)
		if err != nil {
			return invalid(err)
		}
		if p.ScopeChatID == 0 {
			err = c.service.SetGlobalSetting(ctx, p.Field, value, adminID)
		} else {
			err = c.service.SetChatSetting(ctx, p.ScopeChatID, p.Field, value, adminID)
		}
		if err != nil {
			return err
		}
		s.dropPending()
		s.Screen.Notice = "Saved."
		s.MarkRerender()
		deletePromptAndReply()

		return nil

	case PendingSearchInput:
		text, err := textOnly()
		if err != nil {
			return invalid(err)
		}
		if text == "" {
			return invalid(errors.New("the search query must not be empty; send - to clear"))
		}
		if text == "-" {
			text = ""
		}
		s.dropPending()
		deletePromptAndReply()
		if s.Screen.Kind == ScreenCatalog {
			s.Screen.Search = text
			s.Screen.Page = 1
			s.Screen.Notice = ""
		} else {
			s.PushScreen(ScreenState{Kind: ScreenCatalog, Search: text})
		}
		s.MarkRerender()

		return nil

	case PendingPromptText:
		if in.Document != nil {
			return invalid(errors.New("send a text message, not a document"))
		}
		// Prompt bodies are stored verbatim: trimming is validation only
		// (whitespace-only means empty — the service rejects it below).
		if strings.TrimSpace(in.Text) == "" {
			return invalid(errors.New("the template must not be empty"))
		}
		if len(in.Text) > PromptBodyLimit {
			return invalid(fmt.Errorf("the template exceeds %d bytes", PromptBodyLimit))
		}
		if err := c.service.ValidatePromptBody(p.Feature, in.Text); err != nil {
			return invalid(err)
		}
		if err := c.service.SetPrompt(ctx, p.Feature, p.ScopeChatID, in.Text, adminID); err != nil {
			return err
		}
		s.dropPending()
		s.Screen.Notice = "Template saved."
		s.MarkRerender()
		deletePromptAndReply()

		return nil

	case PendingDocumentUpload:
		doc := in.Document
		switch {
		case doc == nil:
			return invalid(errors.New("reply with a .txt or .md document"))
		case !hasPromptFileExtension(doc.Filename):
			return invalid(errors.New("only .txt and .md files are accepted"))
		case len(doc.Content) > PromptBodyLimit:
			return invalid(fmt.Errorf("the document exceeds %d bytes", PromptBodyLimit))
		case !utf8.Valid(doc.Content):
			return invalid(errors.New("the document must be UTF-8 text"))
		}
		if err := c.service.ValidatePromptBody(p.Feature, string(doc.Content)); err != nil {
			return invalid(err)
		}
		if err := c.service.SetPrompt(ctx, p.Feature, p.ScopeChatID, string(doc.Content), adminID); err != nil {
			return err
		}
		s.dropPending()
		s.Screen.Notice = "Template saved."
		s.MarkRerender()
		deletePromptAndReply()

		return nil

	case PendingManualChatID:
		if in.Document != nil {
			return invalid(errors.New("send the numeric chat ID as text"))
		}
		chatID, err := strconv.ParseInt(strings.TrimSpace(in.Text), 10, 64)
		if err != nil {
			return invalid(errors.New("send a numeric chat ID (group chats are negative)"))
		}
		beginChatCompletion(p, chatID)
		deletePromptAndReply()
		fx.verify = &VerifyChat{ChatID: chatID}

		return nil

	case PendingChatPicker:
		// The picker helper message also accepts a numeric reply as the
		// manual chat-ID fallback. The reply keyboard dies with the selection.
		if in.Document != nil {
			return invalid(errors.New("select a chat with the keyboard button, or reply with a numeric chat ID"))
		}
		chatID, err := strconv.ParseInt(strings.TrimSpace(in.Text), 10, 64)
		if err != nil {
			return invalid(errors.New("select a chat with the keyboard button, or reply with a numeric chat ID"))
		}
		s.orphanPickerKeyboard()
		beginChatCompletion(p, chatID)
		deletePromptAndReply()
		fx.verify = &VerifyChat{ChatID: chatID}

		return nil

	default:
		fx.message = unmatchedFeedback(in)

		return nil
	}
}

// beginChatCompletion moves a pending input into the "awaiting GetChat
// completion" state shared by the picker and manual-ID flows: no further
// replies or chat_shared events match it, CompleteChatAdd expects exactly
// this state.
func beginChatCompletion(p *Pending, chatID int64) {
	p.Kind = PendingChatPicker
	p.RequestID = 0
	p.ScopeChatID = chatID
	p.PromptMsgID = 0
}

func (c *Controller) retryPrompt(ctx context.Context, p *Pending, warning error) (string, string) {
	var text, placeholder string
	switch p.Kind {
	case PendingStringInput:
		if spec, err := fieldSpecFor(p.Field, p.ScopeChatID); err == nil {
			current, _ := c.currentFieldValue(ctx, p.ScopeChatID, p.Field)
			text = stringInputPrompt(spec, current)
			placeholder = "New " + spec.Label
		}
	case PendingSearchInput:
		text, placeholder = searchInputPrompt(), "Search query"
	case PendingPromptText:
		text = promptTextInputPrompt(p.Feature, p.ScopeChatID)
	case PendingDocumentUpload:
		text = promptUploadPrompt(p.Feature, p.ScopeChatID)
	case PendingManualChatID:
		text, placeholder = manualChatIDPrompt(), "Chat ID"
	case PendingChatPicker:
		text = pickerPromptText()
	}

	return fmt.Sprintf("%s\n\n⚠️ %v\n\nTry again (attempt %d of %d).", text, warning, p.Attempts, MaxInputAttempts), placeholder
}

// buildScreen builds the presentation screen for a screen state from fresh
// store queries. It runs after the commit; the render gate guarantees the
// newest committed state is what ends up visible.
func (c *Controller) buildScreen(ctx context.Context, seq uint32, state ScreenState) (*Screen, error) {
	b := screenBuilder{seq: seq}

	var screen *Screen
	switch state.Kind {
	case ScreenDashboard:
		summary, err := c.service.AccessSummary(ctx)
		if err != nil {
			return nil, err
		}
		recent, err := c.service.RecentChats(ctx, 3, c.adminIDs)
		if err != nil {
			return nil, err
		}
		screen = b.dashboard(DashboardData{Summary: summary, RecentChats: recent, Uptime: c.manager.Now().Sub(c.started)})

	case ScreenCatalog:
		page, err := c.service.QueryChats(ctx, adminconfig.ChatQuery{Filter: state.Filter, Search: state.Search, Page: state.Page})
		if err != nil {
			return nil, err
		}
		screen = b.catalog(CatalogData{Page: page, Filter: state.Filter, Search: state.Search})

	case ScreenChatCard:
		view, err := c.service.EffectiveChatConfig(ctx, state.ChatID)
		if err != nil {
			return nil, err
		}
		policy, err := c.service.AccessPolicy(ctx)
		if err != nil {
			return nil, err
		}
		global, err := c.service.Store().GetGlobalSettings(ctx)
		if err != nil {
			return nil, err
		}
		entry, found := c.catalogEntry(ctx, state.ChatID)

		data := ChatCardData{
			Info:              entry,
			Found:             found,
			View:              view,
			AccessLine:        accessStatusLabel(policy, view.Allowlisted),
			HidePreviewFields: !global.ShowReplyPreviews,
		}
		for _, mode := range adminconfig.InteractivityModes() {
			data.ModeValues = append(data.ModeValues, string(mode))
			data.ModeLabels = append(data.ModeLabels, adminconfig.ModeShortLabel(mode))
		}
		for _, field := range view.Fields {
			if field.Spec.Name == "interactivity_mode" {
				data.Interactivity = field.Effective
				data.ModeOverridden = field.Overridden
			}
		}
		screen = b.chatCard(data)

	case ScreenGlobalSet:
		view, err := c.service.EffectiveGlobalConfig(ctx)
		if err != nil {
			return nil, err
		}
		screen = b.settings(SettingsData{Global: true, View: view})

	case ScreenChatSet:
		view, err := c.service.EffectiveChatConfig(ctx, state.ChatID)
		if err != nil {
			return nil, err
		}
		global, err := c.service.Store().GetGlobalSettings(ctx)
		if err != nil {
			return nil, err
		}
		screen = b.settings(SettingsData{
			ChatView:          view,
			ChatName:          c.chatDisplayName(ctx, state.ChatID),
			HidePreviewFields: !global.ShowReplyPreviews,
		})

	case ScreenFieldEditor:
		data, err := c.fieldEditorData(ctx, state)
		if err != nil {
			return nil, err
		}
		screen = b.fieldEditor(data)

	case ScreenPrompts:
		overview, err := c.service.PromptOverview(ctx, state.ChatID)
		if err != nil {
			return nil, err
		}
		screen = b.promptsMenu(PromptsMenuData{
			ChatScope: state.ChatID,
			ChatName:  c.chatDisplayName(ctx, state.ChatID),
			Overview:  overview,
		})

	case ScreenPromptDetail:
		body, source, err := c.service.ActivePrompt(ctx, state.Feature, state.ChatID)
		if err != nil {
			return nil, err
		}
		overview, err := c.service.PromptOverview(ctx, state.ChatID)
		if err != nil {
			return nil, err
		}
		status := adminconfig.PromptStatus{Feature: state.Feature}
		for _, entry := range overview {
			if entry.Feature == state.Feature {
				status = entry
			}
		}
		builtin, hasBuiltin := c.service.BuiltinPrompt(state.Feature)
		screen = b.promptDetail(PromptDetailData{
			Feature:      state.Feature,
			ChatScope:    state.ChatID,
			ChatName:     c.chatDisplayName(ctx, state.ChatID),
			Status:       status,
			ActiveBody:   body,
			ActiveSource: source,
			BuiltinBody:  builtin,
			HasBuiltin:   hasBuiltin,
		})

	case ScreenAccess:
		summary, err := c.service.AccessSummary(ctx)
		if err != nil {
			return nil, err
		}
		entries, err := c.service.Allowlist(ctx)
		if err != nil {
			return nil, err
		}
		page := state.Page
		if page < 1 {
			page = 1
		}
		// The allowlist is unbounded; paginate like the catalog so the
		// screen always fits Telegram's message and keyboard limits.
		pages := (len(entries) + adminconfig.DefaultChatPageSize - 1) / adminconfig.DefaultChatPageSize
		if pages > 0 && page > pages {
			page = pages
		}
		start := (page - 1) * adminconfig.DefaultChatPageSize
		end := start + adminconfig.DefaultChatPageSize
		if end > len(entries) {
			end = len(entries)
		}
		screen = b.access(AccessData{
			Summary: summary,
			Entries: entries[start:end],
			Total:   len(entries),
			Page:    page,
			Pages:   pages,
		})

	case ScreenStatistics:
		snapshot := "Statistics are not available."
		if c.stats != nil {
			snapshot = c.stats()
		}
		screen = b.statistics(StatisticsData{Snapshot: snapshot})

	case ScreenConfirm:
		screen = b.confirm(ConfirmData{Description: state.Detail, Danger: state.Danger})

	default:
		return nil, fmt.Errorf("unknown screen kind %q", state.Kind)
	}

	if state.Notice != "" {
		screen.Text = "⚠️ " + state.Notice + "\n\n" + screen.Text
	}

	return screen, nil
}

func (c *Controller) fieldEditorData(ctx context.Context, state ScreenState) (FieldEditorData, error) {
	spec, err := fieldSpecFor(state.Field, state.ChatID)
	if err != nil {
		return FieldEditorData{}, err
	}

	data := FieldEditorData{Global: state.ChatID == 0, ScopeChat: state.ChatID, Spec: spec}
	if state.ChatID == 0 {
		settings, err := c.service.Store().GetGlobalSettings(ctx)
		if err != nil {
			return FieldEditorData{}, err
		}
		data.Value = globalSettingsValue(settings, spec)
	} else {
		view, err := c.service.EffectiveChatConfig(ctx, state.ChatID)
		if err != nil {
			return FieldEditorData{}, err
		}
		for _, field := range view.Fields {
			if field.Spec.Name == state.Field {
				data.Value = field.Effective
				data.Overridden = field.Overridden
			}
		}
	}

	return data, nil
}

func (c *Controller) currentFieldValue(ctx context.Context, scopeChatID int64, field string) (string, error) {
	spec, err := fieldSpecFor(field, scopeChatID)
	if err != nil {
		return "", err
	}

	if scopeChatID == 0 {
		settings, err := c.service.Store().GetGlobalSettings(ctx)
		if err != nil {
			return "", err
		}

		return globalSettingsValue(settings, spec), nil
	}

	view, err := c.service.EffectiveChatConfig(ctx, scopeChatID)
	if err != nil {
		return "", err
	}
	for _, entry := range view.Fields {
		if entry.Spec.Name == field {
			return entry.Effective, nil
		}
	}

	return "", nil
}

func (c *Controller) catalogEntry(ctx context.Context, chatID int64) (adminconfig.ChatCatalogEntry, bool) {
	chats, err := c.service.ChatList(ctx)
	if err != nil {
		return adminconfig.ChatCatalogEntry{}, false
	}
	for _, chat := range chats {
		if chat.ChatID == chatID {
			return chat, true
		}
	}

	return adminconfig.ChatCatalogEntry{}, false
}

func (c *Controller) chatDisplayName(ctx context.Context, chatID int64) string {
	if entry, found := c.catalogEntry(ctx, chatID); found {
		return ChatEntryName(entry)
	}

	return strconv.FormatInt(chatID, 10)
}

func (c *Controller) policySwitchDescription(ctx context.Context, policy adminconfig.AccessPolicy) (string, bool, error) {
	if policy == adminconfig.AccessPolicyAllowAll {
		return "Switch the access policy to allow_all?\n\n" +
			"EVERY chat will be able to use the bot. Allowlist membership is kept but not enforced. " +
			"Reminders will deliver to all chats again.", true, nil
	}

	summary, err := c.service.AccessSummary(ctx)
	if err != nil {
		return "", false, err
	}

	description := "Switch the access policy to allowlist?\n\n" +
		"Only allowlisted chats and admin DMs will be served. Reminders to other chats stop delivering."
	if summary.AllowlistCount == 0 {
		description += "\n\n⚠️ The allowlist is EMPTY: all ordinary chats will be denied."
	}

	return description, false, nil
}

func confirmState(description string, danger bool, chatID int64, feature adminconfig.PromptFeature) ScreenState {
	return ScreenState{Kind: ScreenConfirm, ChatID: chatID, Feature: feature, Detail: description, Danger: danger}
}

// rerenderInPlace refreshes the current screen, skipping the render entirely
// when the state is already clean and unchanged: a byte-identical re-render
// is a pure no-op (no API call, no sequence bump — Telegram would reject the
// identical edit anyway).
func rerenderInPlace(s *Session) error {
	if s.Screen.Notice == "" {
		return nil
	}
	s.Screen.Notice = ""
	s.MarkRerender()

	return nil
}

// scopeFromScreen reports the chat scope (0 = global) the pressed button
// belongs to, derived from the screen it was rendered on.
func scopeFromScreen(s *Session) int64 {
	switch s.Screen.Kind {
	case ScreenChatCard, ScreenChatSet, ScreenFieldEditor, ScreenPrompts, ScreenPromptDetail:
		return s.Screen.ChatID
	default:
		return 0
	}
}

func fieldFromArgs(s *Session, action Action) (string, adminconfig.FieldSpec, error) {
	if len(action.Args) < 1 {
		return "", adminconfig.FieldSpec{}, errInvalidArg
	}

	scope := scopeFromScreen(s)
	name, ok := FieldName(action.Args[0], scope == 0)
	if !ok {
		return "", adminconfig.FieldSpec{}, errInvalidArg
	}

	spec, err := fieldSpecFor(name, scope)
	if err != nil {
		return "", adminconfig.FieldSpec{}, errInvalidArg
	}

	return name, spec, nil
}

func featureFromArgs(action Action) (adminconfig.PromptFeature, bool) {
	if len(action.Args) != 1 {
		return "", false
	}

	feature, ok := PromptFeatureFromCode(action.Args[0])

	return feature, ok
}

func fieldSpecFor(name string, scopeChatID int64) (adminconfig.FieldSpec, error) {
	specs := adminconfig.ChatFieldSpecs()
	if scopeChatID == 0 {
		specs = adminconfig.GlobalFieldSpecs()
	}
	for _, spec := range specs {
		if spec.Name == name {
			return spec, nil
		}
	}

	return adminconfig.FieldSpec{}, fmt.Errorf("unknown settings field %q", name)
}

func parseCatalogFilter(arg string) (adminconfig.ChatFilter, bool) {
	switch arg {
	case string(adminconfig.ChatFilterAll):
		return adminconfig.ChatFilterAll, true
	case string(adminconfig.ChatFilterGroups), "gr":
		return adminconfig.ChatFilterGroups, true
	case string(adminconfig.ChatFilterPrivate), "pr":
		return adminconfig.ChatFilterPrivate, true
	default:
		return adminconfig.ChatFilterAll, false
	}
}

func accessStatusLabel(policy adminconfig.AccessPolicy, allowlisted bool) string {
	switch {
	case policy == adminconfig.AccessPolicyAllowlist && allowlisted:
		return "allowlisted"
	case policy == adminconfig.AccessPolicyAllowlist:
		return "blocked (allowlist policy)"
	case allowlisted:
		return "allowlisted (policy already allows all)"
	default:
		return "allowed via allow_all (not allowlisted)"
	}
}

func promptDocument(feature adminconfig.PromptFeature, body string) *DocumentPayload {
	return &DocumentPayload{Filename: string(feature) + "_template.txt", Content: []byte(body)}
}

func hasPromptFileExtension(filename string) bool {
	lower := strings.ToLower(filename)
	dot := strings.LastIndex(lower, ".")
	if dot < 0 {
		return false
	}

	switch lower[dot:] {
	case ".txt", ".md":
		return true
	default:
		return false
	}
}

func unmatchedFeedback(in ReplyInput) *TextMessage {
	if !in.TextAvailable {
		return &TextMessage{Text: msgUnmatchedReply}
	}

	return &TextMessage{Text: msgInputInactive}
}

func stringInputPrompt(spec adminconfig.FieldSpec, current string) string {
	text := AdminInputMarker + " — " + spec.Label + "\n\nSend the new value as a reply to this message."
	if current != "" {
		text += "\nCurrent: " + current
	}
	if spec.MaxLen > 0 {
		text += fmt.Sprintf("\nUp to %d bytes.", spec.MaxLen)
	}

	return text
}

func searchInputPrompt() string {
	return AdminInputMarker + " — Chat search\n\n" +
		"Send the search query as a reply (title, @username, or exact chat ID). Send - to clear the search."
}

func promptTextInputPrompt(feature adminconfig.PromptFeature, scopeChatID int64) string {
	return AdminInputMarker + " — " + promptFeatureLabel(feature) + " template (" + promptScopeLabel(scopeChatID) + ")\n\n" +
		"Send the complete new template as a reply to this message. " +
		"Tip: download the current template first and edit it; placeholders like {{.Language}} stay intact."
}

func promptUploadPrompt(feature adminconfig.PromptFeature, scopeChatID int64) string {
	return AdminInputMarker + " — " + promptFeatureLabel(feature) + " template upload (" + promptScopeLabel(scopeChatID) + ")\n\n" +
		"Reply to this message with a .txt or .md document (UTF-8, at most 32 KiB)."
}

func manualChatIDPrompt() string {
	return AdminInputMarker + " — Add chat by ID\n\n" +
		"Send the numeric chat ID as a reply (group chats are negative, e.g. -1001234567890). " +
		"The bot will verify the chat via GetChat; unreachable chats are added as unverified."
}

func pickerPromptText() string {
	return AdminInputMarker + " — Add chat\n\n" +
		"Use the shared-chat button below to pick a chat (the bot does not need to be a member). " +
		"Alternatively reply to this message with the numeric chat ID."
}

func promptScopeLabel(scopeChatID int64) string {
	if scopeChatID == 0 {
		return "global"
	}

	return "chat"
}

func randomRequestID() int {
	var buffer [4]byte
	if _, err := rand.Read(buffer[:]); err != nil {
		return 1
	}

	id := int(binary.BigEndian.Uint32(buffer[:]) & 0x7fffffff)
	if id == 0 {
		return 1
	}

	return id
}
