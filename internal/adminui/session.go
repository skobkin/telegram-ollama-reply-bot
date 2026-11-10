package adminui

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"sync"
	"time"

	"telegram-ollama-reply-bot/internal/adminconfig"
)

// Session lifetimes and input limits (chosen defaults, see UPGRADE.md).
const (
	// SessionTTL is the sliding inactivity window of an admin session.
	SessionTTL = 30 * time.Minute
	// PendingTTL is the fixed lifetime of a pending input request.
	PendingTTL = 10 * time.Minute
	// MaxInputAttempts bounds retries on invalid input before the pending
	// input is cancelled.
	MaxInputAttempts = 3
	// MaxStackDepth bounds the back-navigation stack.
	MaxStackDepth = 12
)

// Sentinel errors reported by the manager; the controller maps them to
// user-facing answers and notices.
var (
	// ErrSessionGone means no live session exists for the admin (never
	// opened, closed, replaced, or restart-orphaned).
	ErrSessionGone = errors.New("admin session is gone")
	// ErrSessionExpired means the session TTL elapsed.
	ErrSessionExpired = errors.New("admin session expired")
	// ErrSeqStale means the callback's sequence number does not match the
	// session (an old button); the action must not mutate anything.
	ErrSeqStale = errors.New("callback sequence is stale")
	// ErrDetached means the session was replaced by a newer /admin.
	ErrDetached = errors.New("admin session was replaced")
)

// ScreenKind enumerates the admin UI screens.
type ScreenKind string

const (
	ScreenDashboard    ScreenKind = "dashboard"
	ScreenCatalog      ScreenKind = "catalog"
	ScreenChatCard     ScreenKind = "chat_card"
	ScreenGlobalSet    ScreenKind = "global_settings"
	ScreenChatSet      ScreenKind = "chat_settings"
	ScreenFieldEditor  ScreenKind = "field_editor"
	ScreenPrompts      ScreenKind = "prompts"
	ScreenPromptDetail ScreenKind = "prompt_detail"
	ScreenAccess       ScreenKind = "access"
	ScreenStatistics   ScreenKind = "statistics"
	ScreenConfirm      ScreenKind = "confirm"
)

// ScreenState is the data needed to render one screen. It is a value type:
// copies of it are safe to render after the session lock is released.
type ScreenState struct {
	Kind    ScreenKind
	ChatID  int64
	Page    int
	Filter  adminconfig.ChatFilter
	Search  string
	Field   string
	Feature adminconfig.PromptFeature
	// Detail carries the confirm-screen description (built when the
	// confirmation is created, so re-renders need no extra session state).
	Detail string
	// Danger marks a confirmation as destructive (button styling hint).
	Danger bool
	Notice string
}

// PendingKind enumerates awaited admin inputs.
type PendingKind string

const (
	// PendingStringInput awaits a settings field value (ForceReply).
	PendingStringInput PendingKind = "field_string"
	// PendingSearchInput awaits a catalog search query (ForceReply).
	PendingSearchInput PendingKind = "catalog_search"
	// PendingPromptText awaits a prompt body as text (ForceReply).
	PendingPromptText PendingKind = "prompt_text"
	// PendingDocumentUpload awaits a prompt body as a document reply.
	PendingDocumentUpload PendingKind = "prompt_document"
	// PendingManualChatID awaits a numeric chat ID (ForceReply).
	PendingManualChatID PendingKind = "manual_chat_id"
	// PendingChatPicker awaits a KeyboardButtonRequestChat response.
	PendingChatPicker PendingKind = "chat_picker"
)

// Pending is an awaited correlated input.
type Pending struct {
	Kind        PendingKind
	Field       string
	Feature     adminconfig.PromptFeature
	ScopeChatID int64
	RequestID   int
	PromptMsgID int
	ExpiresAt   time.Time
	Attempts    int
}

// Expired reports whether the pending input's fixed lifetime elapsed.
func (p *Pending) Expired(now time.Time) bool {
	return p != nil && now.After(p.ExpiresAt)
}

// ConfirmKind enumerates confirmation-gated destructive operations.
type ConfirmKind string

const (
	ConfirmPolicySwitch  ConfirmKind = "policy_switch"
	ConfirmAllowlistDrop ConfirmKind = "allowlist_remove"
	ConfirmChatOverrides ConfirmKind = "chat_overrides_reset"
	ConfirmChatPrompt    ConfirmKind = "chat_prompt_reset"
	ConfirmGlobalPrompt  ConfirmKind = "global_prompt_restore"
)

// PendingConfirm is a stored server-side confirmation. Re-validated at
// execution time and cleared on navigation.
type PendingConfirm struct {
	Kind        ConfirmKind
	ChatID      int64
	Policy      adminconfig.AccessPolicy
	Feature     adminconfig.PromptFeature
	Description string
	Danger      bool
}

// Session is one admin's ephemeral UI state. All mutation happens under mu
// inside Manager.Apply; nothing else locks it.
type Session struct {
	mu       sync.Mutex
	detached bool
	rerender bool
	consumed bool

	AdminID   int64
	MsgID     int
	Seq       uint32
	Screen    ScreenState
	Stack     []ScreenState
	Pending   *Pending
	Confirm   *PendingConfirm
	ExpiresAt time.Time

	// Epoch is the render epoch assigned to the latest committed screen
	// change; renders carrying older epochs are skipped.
	Epoch uint64
}

// Render is a committed screen snapshot plus the epoch that orders it. Built
// is the presentation-ready screen for the state, attached by the controller
// after the commit; the adapter renders it through the render gate.
type Render struct {
	AdminID int64
	Epoch   uint64
	MsgID   int
	Screen  ScreenState
	Built   *Screen
}

// renderGate serializes the visible message updates of one admin so an
// older render can never overwrite a newer one.
type renderGate struct {
	mu          sync.Mutex
	lastApplied uint64
}

// Manager owns the per-admin sessions. mu guards ONLY the sessions map and
// the epoch counter (short operations); session mutation and the store IO
// committed inside Apply run under the per-session lock, so unrelated admins
// never contend.
type Manager struct {
	mu       sync.Mutex
	sessions map[int64]*Session
	gates    map[int64]*renderGate
	epoch    uint64
	now      func() time.Time
}

// NewManager creates a session manager. now is injectable for tests; nil
// uses wall time.
func NewManager(now func() time.Time) *Manager {
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}

	return &Manager{
		sessions: make(map[int64]*Session),
		gates:    make(map[int64]*renderGate),
		now:      now,
	}
}

// Now exposes the manager clock to the controller.
func (m *Manager) Now() time.Time {
	return m.now()
}

func randomSeq() uint32 {
	var buffer [4]byte
	if _, err := rand.Read(buffer[:]); err != nil {
		return 1
	}

	seq := binary.BigEndian.Uint32(buffer[:])
	if seq == 0 {
		return 1
	}

	return seq
}

// Replace opens a fresh session for the admin, detaching any previous one.
// The replacement is linearized: after Replace returns, no in-flight action
// of the OLD session can commit a mutation — it either finished before the
// old session's lock was taken here, or it observes detached and aborts.
func (m *Manager) Replace(adminID int64, initial ScreenState) *Session {
	m.mu.Lock()

	old := m.sessions[adminID]

	session := &Session{
		AdminID:   adminID,
		Seq:       randomSeq(),
		Screen:    initial,
		ExpiresAt: m.now().Add(SessionTTL),
	}
	session.Epoch = m.nextEpochLocked()

	m.sessions[adminID] = session
	m.mu.Unlock()

	if old != nil {
		old.mu.Lock()
		old.detached = true
		old.mu.Unlock()
	}

	return session
}

// Session returns the live session for the admin, or nil. The caller must
// not hold the returned session's lock.
func (m *Manager) Session(adminID int64) *Session {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.sessions[adminID]
}

// Close drops the admin's session (detaching it), used by /cancel.
func (m *Manager) Close(adminID int64) {
	m.mu.Lock()
	old := m.sessions[adminID]
	delete(m.sessions, adminID)
	m.mu.Unlock()

	if old != nil {
		old.mu.Lock()
		old.detached = true
		old.mu.Unlock()
	}
}

// ErrApply reports why an Apply could not run. Apply itself never performs
// Telegram IO; the controller adapts the results.
type ApplyResult struct {
	Session *Session
	Render  *Render
}

// Apply runs fn atomically against the admin's session:
//
//   - the session must exist, not be detached, and not be expired;
//   - for callback actions (seq > 0) the sequence must match the session's
//     current Seq, otherwise ErrSeqStale (no mutation);
//   - fn runs under the session lock (store IO allowed, Telegram IO
//     forbidden) and signals its outcome on the session: MarkRerender when
//     the screen changed (Seq bumps, a fresh render epoch is assigned, a
//     Render snapshot is produced), MarkConsumed for observable actions
//     without a screen change (Seq bumps so replaying the same button is
//     stale), or neither for pure no-ops (nothing changes);
//   - an error return aborts the commit — fn must arrange to touch session
//     state only after its own operations succeed (the controller's
//     discipline: mutate the store first, the session last).
func (m *Manager) Apply(adminID int64, seq uint32, fn func(s *Session) error) (ApplyResult, error) {
	session := m.Session(adminID)
	if session == nil {
		return ApplyResult{}, ErrSessionGone
	}

	session.mu.Lock()
	defer session.mu.Unlock()

	if session.detached {
		return ApplyResult{}, ErrDetached
	}
	if m.now().After(session.ExpiresAt) {
		return ApplyResult{}, ErrSessionExpired
	}
	if seq != 0 && seq != session.Seq {
		return ApplyResult{}, ErrSeqStale
	}

	err := fn(session)
	rerender := session.rerender
	consumed := session.consumed
	session.rerender = false
	session.consumed = false
	if err != nil {
		return ApplyResult{}, err
	}

	if rerender || consumed {
		session.Seq++
	}
	if rerender {
		m.mu.Lock()
		session.Epoch = m.nextEpochLocked()
		m.mu.Unlock()
		session.ExpiresAt = m.now().Add(SessionTTL)
	}

	return ApplyResult{Session: session, Render: renderOf(session, rerender)}, nil
}

// MarkRerender flags the session for a screen refresh on commit.
func (s *Session) MarkRerender() { s.rerender = true }

// MarkConsumed flags an observable action without a screen change (for
// example a document download): the sequence bumps so the pressed button
// cannot be replayed, but no re-render is produced.
func (s *Session) MarkConsumed() { s.consumed = true }

// PushScreen navigates: the current screen goes onto the back stack (capped)
// and the new screen becomes current.
func (s *Session) PushScreen(next ScreenState) {
	if len(s.Stack) >= MaxStackDepth {
		s.Stack = s.Stack[1:]
	}
	s.Stack = append(s.Stack, s.Screen)
	s.Screen = next
	s.Pending = nil
	s.Confirm = nil
}

// PopScreen navigates back; when the stack is empty the screen stays.
func (s *Session) PopScreen() {
	if len(s.Stack) == 0 {
		return
	}
	s.Screen = s.Stack[len(s.Stack)-1]
	s.Stack = s.Stack[:len(s.Stack)-1]
	s.Pending = nil
	s.Confirm = nil
}

// DetachedSnapshot reports (under the lock) whether the session is detached
// and its current screen; renderers use it to drop late renders.
func (s *Session) DetachedSnapshot() (bool, ScreenState, int, uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.detached, s.Screen, s.MsgID, s.Epoch
}

func renderOf(session *Session, include bool) *Render {
	if !include {
		return nil
	}

	return &Render{
		AdminID: session.AdminID,
		Epoch:   session.Epoch,
		MsgID:   session.MsgID,
		Screen:  session.Screen,
	}
}

func (m *Manager) nextEpochLocked() uint64 {
	m.epoch++

	return m.epoch
}

// ApplyRender performs one visible message update through the admin's
// render gate: older epochs than the last applied one are skipped, renders
// of detached sessions are dropped, and a successful edit advances the gate.
// The edit callback must not touch sessions.
func (m *Manager) ApplyRender(render *Render, session *Session, edit func() error) error {
	if render == nil {
		return nil
	}

	gate := m.gateFor(render.AdminID)

	gate.mu.Lock()
	defer gate.mu.Unlock()

	if render.Epoch <= gate.lastApplied {
		return nil
	}

	if session != nil {
		session.mu.Lock()
		detached := session.detached
		session.mu.Unlock()
		if detached {
			return nil
		}
	}

	if err := edit(); err != nil {
		return err
	}

	gate.lastApplied = render.Epoch

	return nil
}

func (m *Manager) gateFor(adminID int64) *renderGate {
	m.mu.Lock()
	defer m.mu.Unlock()

	gate, ok := m.gates[adminID]
	if !ok {
		gate = &renderGate{}
		m.gates[adminID] = gate
	}

	return gate
}

// AdoptMsgID records a new screen message ID (after a resend) under the
// session lock. No-ops for missing sessions.
func (m *Manager) AdoptMsgID(adminID int64, msgID int) {
	session := m.Session(adminID)
	if session == nil {
		return
	}

	session.mu.Lock()
	session.MsgID = msgID
	session.mu.Unlock()
}

// SetPendingPromptMsgID records the message ID of a sent input prompt as the
// very next statement after the API call, closing the correlation race (a
// pending is never matched before its prompt message ID is known).
func (m *Manager) SetPendingPromptMsgID(adminID int64, msgID int) error {
	_, err := m.Apply(adminID, 0, func(s *Session) error {
		if s.Pending == nil {
			return ErrSessionGone
		}
		if msgID == 0 {
			return errors.New("prompt message id must not be zero")
		}
		s.Pending.PromptMsgID = msgID

		return nil
	})

	return err
}
