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
	// ErrPendingReplaced means the pending input a prompt send tried to
	// correlate is no longer the live, awaiting pending — the send is
	// dropped rather than correlating the wrong prompt message.
	ErrPendingReplaced = errors.New("pending input was replaced")
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

// Pending is an awaited correlated input. ID identifies one pending within
// its session: prompt sends correlate by it, so a late send for a pending
// that was already replaced can never stamp the wrong message ID onto the
// live one.
type Pending struct {
	ID          uint64
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

	AdminID    int64
	MsgID      int
	Seq        uint32
	Screen     ScreenState
	Stack      []ScreenState
	Pending    *Pending
	Confirm    *PendingConfirm
	ExpiresAt  time.Time
	pendingSeq uint64

	// Epoch is the render epoch assigned to the latest committed screen
	// change; renders carrying older epochs are skipped.
	Epoch uint64
}

// Render is a committed screen snapshot plus the epoch that orders it. Built
// is the presentation-ready screen for the state, attached by the controller
// after the commit; the adapter renders it through the render gate. Seq is
// captured under the session lock at commit time so button data built after
// the lock is released never races a concurrent commit.
type Render struct {
	AdminID int64
	Epoch   uint64
	Seq     uint32
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

// ActiveSession returns the admin's session only when it exists, is not
// detached, and has not expired — the adapter's fast validation before it
// acknowledges a callback.
func (m *Manager) ActiveSession(adminID int64) (*Session, bool) {
	session := m.Session(adminID)
	if session == nil {
		return nil, false
	}

	session.mu.Lock()
	defer session.mu.Unlock()

	if session.detached || m.now().After(session.ExpiresAt) {
		return nil, false
	}

	return session, true
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
//     forbidden) and signals a screen change via MarkRerender (Seq bumps, a
//     fresh render epoch is assigned, a Render snapshot is produced); every
//     other outcome leaves Seq alone — observable-but-idempotent actions
//     (document downloads, prompt views) must keep the already-visible
//     keyboard valid, and replay protection for real mutations comes from
//     the store write itself plus the Seq the NEXT render mints;
//   - every successful commit refreshes the sliding TTL (the documented
//     inactivity window), so read-only interactions keep the session alive
//     without changing Seq; failed and stale-seq applies do not;
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
	session.rerender = false
	if err != nil {
		return ApplyResult{}, err
	}

	if rerender {
		session.Seq++
		m.mu.Lock()
		session.Epoch = m.nextEpochLocked()
		m.mu.Unlock()
	}
	// Sliding TTL: every commit reaching this point succeeded — failed and
	// stale-seq applies returned above — so every successful interaction is
	// activity, including read-only actions that leave Seq, and the visible
	// keyboard, untouched.
	session.ExpiresAt = m.now().Add(SessionTTL)

	return ApplyResult{Session: session, Render: renderOf(session, rerender)}, nil
}

// MarkRerender flags the session for a screen refresh on commit.
func (s *Session) MarkRerender() { s.rerender = true }

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

// Snapshot returns the render-relevant state (under the lock): the sequence
// buttons must encode, the committed screen, the menu message ID, and the
// render epoch. OpenDashboard uses it right after publishing a fresh
// session, when concurrent Apply commits may mutate the fields at any time.
func (s *Session) Snapshot() (uint32, ScreenState, int, uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.Seq, s.Screen, s.MsgID, s.Epoch
}

// NextPendingID mints the identity of the next pending input (call under the
// session lock, inside Apply).
func (s *Session) NextPendingID() uint64 {
	s.pendingSeq++

	return s.pendingSeq
}

func renderOf(session *Session, include bool) *Render {
	if !include {
		return nil
	}

	return &Render{
		AdminID: session.AdminID,
		Epoch:   session.Epoch,
		Seq:     session.Seq,
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

// AdoptMsgID records a new screen message ID (after a successful edit or
// resend) — but only on the session the render belongs to. A Telegram call
// that completes after /admin replaced the session must never redirect the
// replacement's menu to the old session's message. No-ops for missing or
// replaced sessions.
func (m *Manager) AdoptMsgID(session *Session, msgID int) {
	if session == nil {
		return
	}

	_, _ = m.Apply(session.AdminID, 0, func(s *Session) error {
		if s != session {
			return ErrDetached
		}
		s.MsgID = msgID

		return nil
	})
}

// SetPendingPromptMsgID records the message ID of a sent input prompt as the
// very next statement after the API call. It stamps ONLY the pending of the
// session the request belongs to AND whose identity the request carried: if
// the admin already replaced that pending (double-tap on another field), it
// moved on (chat selection consumed), or /admin replaced the whole session
// while the send was in flight, the late send is dropped instead of
// correlating the wrong prompt — per-session pending IDs restart at 1, so
// the identity check is what prevents a colliding cross-session stamp.
// Pendings are stamped at most once.
func (m *Manager) SetPendingPromptMsgID(session *Session, pendingID uint64, msgID int) error {
	if session == nil {
		return ErrPendingReplaced
	}

	_, err := m.Apply(session.AdminID, 0, func(s *Session) error {
		if s != session {
			return ErrPendingReplaced
		}
		p := s.Pending
		switch {
		case p == nil, p.ID != pendingID:
			return ErrPendingReplaced
		case p.PromptMsgID != 0:
			return ErrPendingReplaced
		case p.Kind == PendingChatPicker && p.RequestID == 0:
			// Already morphed into the GetChat-completion state; its picker
			// prompt is no longer an input target.
			return ErrPendingReplaced
		}
		if msgID == 0 {
			return errors.New("prompt message id must not be zero")
		}
		p.PromptMsgID = msgID

		return nil
	})

	return err
}
