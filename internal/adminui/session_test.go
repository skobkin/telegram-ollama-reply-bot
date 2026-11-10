package adminui

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestReplaceCreatesFreshSessionWithRandomSeq(t *testing.T) {
	m := NewManager(nil)

	first := m.Replace(7, ScreenState{Kind: ScreenDashboard})
	if first.Seq == 0 {
		t.Fatalf("session seq must be randomly initialized and non-zero")
	}
	if first.Epoch == 0 {
		t.Fatalf("initial render epoch must be assigned")
	}

	second := m.Replace(7, ScreenState{Kind: ScreenDashboard})
	if second == first {
		t.Fatalf("/admin must replace, not reuse, the session")
	}
	if second.Seq == first.Seq {
		t.Fatalf("replacement must start a fresh random sequence")
	}
}

func TestApplyValidatesSeq(t *testing.T) {
	m := NewManager(nil)
	session := m.Replace(7, ScreenState{Kind: ScreenDashboard})

	if _, err := m.Apply(7, session.Seq+1, func(*Session) error { return nil }); !errors.Is(err, ErrSeqStale) {
		t.Fatalf("expected stale seq error, got %v", err)
	}

	mutations := 0
	if _, err := m.Apply(7, session.Seq, func(*Session) error {
		mutations++

		return nil
	}); err != nil {
		t.Fatalf("apply with current seq: %v", err)
	}
	if mutations != 1 {
		t.Fatalf("expected exactly one mutation, got %d", mutations)
	}
}

func TestApplyRerenderBumpsSeqAndEpoch(t *testing.T) {
	m := NewManager(nil)
	session := m.Replace(7, ScreenState{Kind: ScreenDashboard})
	oldSeq, oldEpoch := session.Seq, session.Epoch

	result, err := m.Apply(7, session.Seq, func(s *Session) error {
		s.PushScreen(ScreenState{Kind: ScreenCatalog})
		s.MarkRerender()

		return nil
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if session.Seq != oldSeq+1 {
		t.Fatalf("rerender must bump seq: %d → %d", oldSeq, session.Seq)
	}
	if session.Epoch <= oldEpoch {
		t.Fatalf("rerender must assign a fresh epoch")
	}
	if result.Render == nil || result.Render.Screen.Kind != ScreenCatalog {
		t.Fatalf("expected catalog render, got %+v", result.Render)
	}

	// A mutation without a rerender keeps seq and render unchanged.
	before := session.Seq
	result, err = m.Apply(7, session.Seq, func(*Session) error { return nil })
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if session.Seq != before || result.Render != nil {
		t.Fatalf("no-rerender apply must keep seq and produce no render")
	}
}

func TestApplyFnErrorAbortsCommit(t *testing.T) {
	m := NewManager(nil)
	session := m.Replace(7, ScreenState{Kind: ScreenDashboard})
	before := session.Seq

	// fn discipline: session state is touched only after its operations
	// succeed, so an error return leaves the session untouched.
	sentinel := errors.New("boom")
	if _, err := m.Apply(7, session.Seq, func(*Session) error {
		return sentinel
	}); !errors.Is(err, sentinel) {
		t.Fatalf("expected fn error, got %v", err)
	}

	if session.Seq != before {
		t.Fatalf("aborted apply must not bump seq")
	}
	if session.Screen.Kind != ScreenDashboard {
		t.Fatalf("aborted apply must not change the screen")
	}

	// Flags left set by a failing fn are cleared, not leaked.
	if _, err := m.Apply(7, session.Seq, func(s *Session) error {
		s.MarkRerender()

		return sentinel
	}); !errors.Is(err, sentinel) {
		t.Fatalf("expected fn error, got %v", err)
	}
	if session.rerender {
		t.Fatalf("rerender flag must be cleared after apply")
	}
	if session.Seq != before {
		t.Fatalf("aborted apply must not bump seq even with flags set")
	}
}

// TestApplyWithoutRerenderKeepsSeq pins the semantics that keep the visible
// keyboard alive across pure-output actions (prompt views/downloads): an
// apply that signals neither a screen change nor a store mutation must not
// advance the sequence, or every such action would stale-date the buttons
// already on screen.
func TestApplyWithoutRerenderKeepsSeq(t *testing.T) {
	m := NewManager(nil)
	session := m.Replace(7, ScreenState{Kind: ScreenDashboard})
	before := session.Seq

	result, err := m.Apply(7, session.Seq, func(*Session) error { return nil })
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if session.Seq != before {
		t.Fatalf("no-rerender apply must keep the sequence, got %d (was %d)", session.Seq, before)
	}
	if result.Render != nil {
		t.Fatalf("no-rerender apply must not produce a render")
	}
}

func TestApplyMissingSession(t *testing.T) {
	m := NewManager(nil)

	if _, err := m.Apply(42, 0, func(*Session) error { return nil }); !errors.Is(err, ErrSessionGone) {
		t.Fatalf("expected gone error, got %v", err)
	}
}

func TestSessionExpiryIsLazy(t *testing.T) {
	start := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	offset := atomic.Int64{}
	m := NewManager(func() time.Time { return start.Add(time.Duration(offset.Load()) * time.Second) })

	session := m.Replace(7, ScreenState{Kind: ScreenDashboard})

	offset.Store(int64((SessionTTL + time.Minute).Seconds()))

	if _, err := m.Apply(7, session.Seq, func(*Session) error { return nil }); !errors.Is(err, ErrSessionExpired) {
		t.Fatalf("expected expired error, got %v", err)
	}
}

func TestSessionTTLSlidesOnRerender(t *testing.T) {
	start := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	offset := atomic.Int64{}
	m := NewManager(func() time.Time { return start.Add(time.Duration(offset.Load()) * time.Second) })

	session := m.Replace(7, ScreenState{Kind: ScreenDashboard})
	firstExpiry := session.ExpiresAt

	offset.Store(int64((SessionTTL - time.Minute).Seconds()))
	if _, err := m.Apply(7, session.Seq, func(s *Session) error {
		s.MarkRerender()

		return nil
	}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !session.ExpiresAt.After(firstExpiry) {
		t.Fatalf("successful rerender must slide the TTL")
	}
}

func TestCloseDetachesAndDrops(t *testing.T) {
	m := NewManager(nil)
	session := m.Replace(7, ScreenState{Kind: ScreenDashboard})

	m.Close(7)

	if m.Session(7) != nil {
		t.Fatalf("close must drop the session from the map")
	}
	session.mu.Lock()
	detached := session.detached
	session.mu.Unlock()
	if !detached {
		t.Fatalf("close must detach the old session")
	}
}

// TestReplaceLinearizesInFlightActions is the core replacement invariant: an
// action of the OLD session already running when /admin reopens either
// completes before Replace returns, or a later old-session action aborts
// without mutating. Simulated with a blocked store call.
func TestReplaceLinearizesInFlightActions(t *testing.T) {
	m := NewManager(nil)
	old := m.Replace(7, ScreenState{Kind: ScreenDashboard})

	inFlight := make(chan struct{})
	release := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)

	// An old-session action pauses mid-mutation (store IO in progress).
	go func() {
		defer wg.Done()
		_, _ = m.Apply(7, old.Seq, func(s *Session) error {
			s.PushScreen(ScreenState{Kind: ScreenAccess})
			s.MarkRerender()
			close(inFlight)
			<-release

			return nil
		})
	}()

	<-inFlight

	replaceDone := make(chan struct{})
	go func() {
		m.Replace(7, ScreenState{Kind: ScreenDashboard})
		close(replaceDone)
	}()

	// Replace must BLOCK until the in-flight action finishes (it waits on
	// the old session's lock to detach it).
	select {
	case <-replaceDone:
		t.Fatal("replace must not complete while an old action holds the session lock")
	case <-time.After(50 * time.Millisecond):
	}

	close(release)
	<-replaceDone
	wg.Wait()

	// A NEW old-session action (started after the reopen) must abort
	// without mutating. Apply looks up by adminID: the map now holds the NEW
	// session, so the old seq resolves as stale — a safe abort; what matters
	// is that no old-session state is reachable for mutation.
	mutated := false
	_, err := m.Apply(7, old.Seq, func(*Session) error {
		mutated = true

		return nil
	})
	if mutated {
		t.Fatalf("old-session action after reopen must not mutate anything")
	}
	if err == nil {
		t.Fatalf("old-session action after reopen must fail closed")
	}
}

func TestCrossAdminIsolation(t *testing.T) {
	m := NewManager(nil)
	first := m.Replace(7, ScreenState{Kind: ScreenDashboard})
	second := m.Replace(8, ScreenState{Kind: ScreenDashboard})

	// An action for admin 8 must never touch admin 7's session.
	if _, err := m.Apply(8, second.Seq, func(*Session) error { return nil }); err != nil {
		t.Fatalf("apply for admin 8: %v", err)
	}
	if _, err := m.Apply(7, first.Seq, func(*Session) error { return nil }); err != nil {
		t.Fatalf("apply for admin 7 must still work: %v", err)
	}
}

func TestConcurrentSameAdminCallbacksMutateExactlyOncePerSeq(t *testing.T) {
	m := NewManager(nil)
	session := m.Replace(7, ScreenState{Kind: ScreenDashboard})
	seq := session.Seq

	mutations := atomic.Int32{}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = m.Apply(7, seq, func(s *Session) error {
				mutations.Add(1)
				s.MarkRerender() // every real mutation refreshes the screen

				return nil
			})
		}()
	}
	wg.Wait()

	if got := mutations.Load(); got != 1 {
		t.Fatalf("exactly one concurrent mutating callback with the same seq must commit, got %d", got)
	}
}

func TestConcurrentDifferentAdminsBothProceed(t *testing.T) {
	m := NewManager(nil)
	m.Replace(7, ScreenState{Kind: ScreenDashboard})
	m.Replace(8, ScreenState{Kind: ScreenDashboard})

	done := make(chan struct{}, 2)
	for _, adminID := range []int64{7, 8} {
		go func(adminID int64) {
			session := m.Session(adminID)
			_, _ = m.Apply(adminID, session.Seq, func(*Session) error {
				time.Sleep(20 * time.Millisecond)

				return nil
			})
			done <- struct{}{}
		}(adminID)
	}

	timeout := time.After(2 * time.Second)
	for i := 0; i < 2; i++ {
		select {
		case <-done:
		case <-timeout:
			t.Fatal("different-admin actions must proceed concurrently without deadlock")
		}
	}
}

func TestApplyRenderSkipsOlderEpochs(t *testing.T) {
	m := NewManager(nil)
	session := m.Replace(7, ScreenState{Kind: ScreenDashboard})

	applied := make([]uint64, 0, 4)
	edit := func(epoch uint64) func() error {
		return func() error {
			applied = append(applied, epoch)

			return nil
		}
	}

	// Newer epoch applies first.
	newer := &Render{AdminID: 7, Epoch: 10}
	if err := m.ApplyRender(newer, session, edit(10)); err != nil {
		t.Fatalf("apply render: %v", err)
	}

	// The older epoch arriving late is skipped.
	older := &Render{AdminID: 7, Epoch: 9}
	if err := m.ApplyRender(older, session, edit(9)); err != nil {
		t.Fatalf("apply render: %v", err)
	}

	if len(applied) != 1 || applied[0] != 10 {
		t.Fatalf("only the newer render may edit, applied=%v", applied)
	}
}

func TestApplyRenderSkipsDetachedSession(t *testing.T) {
	m := NewManager(nil)
	old := m.Replace(7, ScreenState{Kind: ScreenDashboard})
	fresh := m.Replace(7, ScreenState{Kind: ScreenDashboard})

	edits := 0
	// A late render from the replaced session must never edit the message.
	err := m.ApplyRender(&Render{AdminID: 7, Epoch: old.Epoch}, old, func() error {
		edits++

		return nil
	})
	if err != nil {
		t.Fatalf("apply render: %v", err)
	}
	if edits != 0 {
		t.Fatalf("detached session render must be dropped, got %d edits", edits)
	}

	// The fresh session's render goes through.
	err = m.ApplyRender(&Render{AdminID: 7, Epoch: fresh.Epoch}, fresh, func() error {
		edits++

		return nil
	})
	if err != nil || edits != 1 {
		t.Fatalf("fresh session render must apply, edits=%d err=%v", edits, err)
	}
}

func TestApplyRenderEditFailureKeepsGatePosition(t *testing.T) {
	m := NewManager(nil)
	session := m.Replace(7, ScreenState{Kind: ScreenDashboard})

	failure := errors.New("message to edit not found")
	if err := m.ApplyRender(&Render{AdminID: 7, Epoch: 5}, session, func() error {
		return failure
	}); !errors.Is(err, failure) {
		t.Fatalf("edit failure must surface, got %v", err)
	}

	// The failed epoch was NOT recorded, so a retry of the same epoch can
	// still apply (used by the resend-recovery path).
	applied := false
	if err := m.ApplyRender(&Render{AdminID: 7, Epoch: 5}, session, func() error {
		applied = true

		return nil
	}); err != nil || !applied {
		t.Fatalf("retry after edit failure must apply, applied=%t err=%v", applied, err)
	}
}

func TestPushPopScreenNavigation(t *testing.T) {
	m := NewManager(nil)
	session := m.Replace(7, ScreenState{Kind: ScreenDashboard})

	_, err := m.Apply(7, session.Seq, func(s *Session) error {
		s.PushScreen(ScreenState{Kind: ScreenCatalog, Page: 2})
		s.Pending = &Pending{Kind: PendingSearchInput}
		s.PushScreen(ScreenState{Kind: ScreenChatCard, ChatID: 42})
		if s.Pending != nil {
			t.Fatal("navigation must clear pendings")
		}
		s.MarkRerender()

		return nil
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}

	if session.Screen.Kind != ScreenChatCard || session.Screen.ChatID != 42 {
		t.Fatalf("unexpected screen: %+v", session.Screen)
	}

	_, err = m.Apply(7, session.Seq, func(s *Session) error {
		s.PopScreen()
		s.MarkRerender()

		return nil
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if session.Screen.Kind != ScreenCatalog || session.Screen.Page != 2 {
		t.Fatalf("back navigation must restore the catalog page: %+v", session.Screen)
	}
}

func TestStackDepthCapped(t *testing.T) {
	m := NewManager(nil)
	session := m.Replace(7, ScreenState{Kind: ScreenDashboard})

	_, err := m.Apply(7, session.Seq, func(s *Session) error {
		for i := 0; i < MaxStackDepth+10; i++ {
			s.PushScreen(ScreenState{Kind: ScreenCatalog, Page: i})
		}

		return nil
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(session.Stack) > MaxStackDepth {
		t.Fatalf("stack must stay capped at %d, got %d", MaxStackDepth, len(session.Stack))
	}
}

func TestSetPendingPromptMsgID(t *testing.T) {
	m := NewManager(nil)
	session := m.Replace(7, ScreenState{Kind: ScreenCatalog})

	// Before any prompt exists, recording must fail (no pending); a nil
	// session (defensive) fails too.
	if err := m.SetPendingPromptMsgID(session, 1, 55); err == nil {
		t.Fatalf("recording without a pending must fail")
	}
	if err := m.SetPendingPromptMsgID(nil, 1, 55); err == nil {
		t.Fatalf("recording without a session must fail")
	}

	if _, err := m.Apply(7, session.Seq, func(s *Session) error {
		s.Pending = &Pending{ID: s.NextPendingID(), Kind: PendingStringInput, Field: "language"}

		return nil
	}); err != nil {
		t.Fatalf("apply: %v", err)
	}

	liveID := session.Pending.ID

	if err := m.SetPendingPromptMsgID(session, liveID, 55); err != nil {
		t.Fatalf("record prompt msg id: %v", err)
	}
	if session.Pending.PromptMsgID != 55 {
		t.Fatalf("prompt message id must be recorded, got %d", session.Pending.PromptMsgID)
	}

	if err := m.SetPendingPromptMsgID(session, liveID, 0); err == nil {
		t.Fatalf("zero message id must be rejected (never visible as live)")
	}

	// A send for a pending that was already replaced must never stamp the
	// live one (out-of-order double-tap on two fields).
	replaced := session.Pending.ID + 1
	if _, err := m.Apply(7, session.Seq, func(s *Session) error {
		s.Pending = &Pending{ID: replaced, Kind: PendingStringInput, Field: "tone_mode"}

		return nil
	}); err != nil {
		t.Fatalf("apply replacement: %v", err)
	}
	if err := m.SetPendingPromptMsgID(session, liveID, 77); err == nil {
		t.Fatalf("stamping by a replaced pending's id must fail")
	}
	if session.Pending.PromptMsgID != 0 {
		t.Fatalf("the replacement must stay unstamped, got %d", session.Pending.PromptMsgID)
	}

	// Re-stamping the same pending is refused (one prompt per pending).
	if err := m.SetPendingPromptMsgID(session, replaced, 88); err != nil {
		t.Fatalf("stamp replacement: %v", err)
	}
	if err := m.SetPendingPromptMsgID(session, replaced, 99); err == nil {
		t.Fatalf("double stamping must be refused")
	}
	if session.Pending.PromptMsgID != 88 {
		t.Fatalf("the first stamp must win, got %d", session.Pending.PromptMsgID)
	}
}

// TestSetPendingPromptMsgIDRefusesReplacedSession pins the cross-session
// correlation race: per-session pending IDs restart at 1, so a ForceReply
// send from a session that /admin replaced mid-flight collides with the
// replacement session's own pending numbering. The late send must be dropped
// — stamping it would correlate the wrong prompt message (and a reply to the
// wrong prompt would then write the wrong field).
func TestSetPendingPromptMsgIDRefusesReplacedSession(t *testing.T) {
	m := NewManager(nil)

	arm := func(session *Session) uint64 {
		t.Helper()

		_, err := m.Apply(7, session.Seq, func(s *Session) error {
			s.Pending = &Pending{ID: s.NextPendingID(), Kind: PendingStringInput, Field: "language"}

			return nil
		})
		if err != nil {
			t.Fatalf("arm pending: %v", err)
		}

		session.mu.Lock()
		defer session.mu.Unlock()

		return session.Pending.ID
	}

	old := m.Replace(7, ScreenState{Kind: ScreenCatalog})
	oldID := arm(old)

	fresh := m.Replace(7, ScreenState{Kind: ScreenCatalog})
	freshID := arm(fresh)
	if oldID != freshID {
		t.Fatalf("test premise: pending IDs must collide across sessions, got %d and %d", oldID, freshID)
	}

	// The delayed send of the old session's prompt completes after the
	// replacement armed its own pending with the same ID.
	if err := m.SetPendingPromptMsgID(old, oldID, 111); !errors.Is(err, ErrPendingReplaced) {
		t.Fatalf("late send from the replaced session must be refused, got %v", err)
	}
	fresh.mu.Lock()
	stamped := fresh.Pending.PromptMsgID
	fresh.mu.Unlock()
	if stamped != 0 {
		t.Fatalf("replacement pending must stay unstamped, got %d", stamped)
	}

	// The replacement session's own send still correlates normally.
	if err := m.SetPendingPromptMsgID(fresh, freshID, 222); err != nil {
		t.Fatalf("own send must correlate: %v", err)
	}
	fresh.mu.Lock()
	stamped = fresh.Pending.PromptMsgID
	fresh.mu.Unlock()
	if stamped != 222 {
		t.Fatalf("own send must stamp its message, got %d", stamped)
	}
}

// TestAdoptMsgIDRefusesReplacedSession pins the menu-adoption race: a
// Telegram edit or resend completing after /admin replaced the session must
// not redirect the replacement session's menu message — otherwise the fresh
// session would point at a message its buttons are not attached to (or
// orphan a duplicate live menu).
func TestAdoptMsgIDRefusesReplacedSession(t *testing.T) {
	m := NewManager(nil)
	old := m.Replace(7, ScreenState{Kind: ScreenDashboard})
	old.mu.Lock()
	old.MsgID = 100
	old.mu.Unlock()

	fresh := m.Replace(7, ScreenState{Kind: ScreenDashboard})

	// The old session's in-flight render finishes after the replacement.
	m.AdoptMsgID(old, 500)

	old.mu.Lock()
	oldMsgID := old.MsgID
	old.mu.Unlock()
	fresh.mu.Lock()
	freshMsgID := fresh.MsgID
	fresh.mu.Unlock()
	if freshMsgID != 0 {
		t.Fatalf("replacement session must not adopt a detached session's message, got %d", freshMsgID)
	}
	if oldMsgID != 100 {
		t.Fatalf("refused adoption must not touch the old session either, got %d", oldMsgID)
	}

	// The live session's own adoptions still apply.
	m.AdoptMsgID(fresh, 501)
	fresh.mu.Lock()
	freshMsgID = fresh.MsgID
	fresh.mu.Unlock()
	if freshMsgID != 501 {
		t.Fatalf("live session must adopt its own message, got %d", freshMsgID)
	}

	m.AdoptMsgID(nil, 502) // nil session is a safe no-op
}

func TestPendingExpiry(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	pending := &Pending{ExpiresAt: now.Add(PendingTTL)}

	if pending.Expired(now) {
		t.Fatalf("pending must be live before its ttl")
	}
	if !pending.Expired(now.Add(PendingTTL + time.Second)) {
		t.Fatalf("pending must expire after its ttl")
	}
}
