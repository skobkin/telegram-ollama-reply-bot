package adminui

import (
	"errors"
	"sync"
	"testing"
	"time"
)

func TestRecreateRenderSnapshotsCurrentScreen(t *testing.T) {
	m := NewManager(nil)
	session := m.Replace(7, ScreenState{Kind: ScreenDashboard})
	session.mu.Lock()
	session.PushScreen(ScreenState{Kind: ScreenCatalog})
	session.mu.Unlock()

	// Give the session a live menu message id, as AdoptMsgID would.
	if _, err := m.Apply(7, 0, func(s *Session) error {
		s.MsgID = 55

		return nil
	}); err != nil {
		t.Fatalf("adopt msg id: %v", err)
	}

	seqBefore, _, msgIDBefore, _ := session.Snapshot()
	expiresBefore := sessionExpiresAt(m, session)

	var sentRender *Render
	err := m.RecreateRender(7, func(render *Render, _ *Session) error {
		sentRender = render

		return nil
	})
	if err != nil {
		t.Fatalf("RecreateRender: %v", err)
	}

	if sentRender == nil {
		t.Fatal("expected a render to be handed to send")
	}
	if sentRender.Screen.Kind != ScreenCatalog || sentRender.MsgID != msgIDBefore || sentRender.Seq != seqBefore {
		t.Fatalf("render must snapshot the current screen unchanged: %+v", sentRender)
	}
	if sentRender.Epoch == 0 {
		t.Fatal("render must carry a minted epoch")
	}

	// Seq and TTL are presentation-untouched.
	seqAfter, _, msgIDAfter, _ := session.Snapshot()
	if seqAfter != seqBefore || msgIDAfter != msgIDBefore {
		t.Fatalf("re-creation must not move Seq or MsgID before adoption: %d/%d vs %d/%d", seqAfter, msgIDAfter, seqBefore, msgIDBefore)
	}
	if !sessionExpiresAt(m, session).Equal(expiresBefore) {
		t.Fatalf("re-creation must not slide the session TTL")
	}
}

func TestRecreateRenderMintsFreshEpochEachCall(t *testing.T) {
	m := NewManager(nil)
	m.Replace(7, ScreenState{Kind: ScreenDashboard})

	var epochs []uint64
	for range 2 {
		if err := m.RecreateRender(7, func(render *Render, _ *Session) error {
			epochs = append(epochs, render.Epoch)

			return nil
		}); err != nil {
			t.Fatalf("RecreateRender: %v", err)
		}
	}

	if epochs[1] <= epochs[0] {
		t.Fatalf("each re-creation must mint a strictly newer epoch: %d then %d", epochs[0], epochs[1])
	}
}

func TestRecreateRenderMissingExpiredDetached(t *testing.T) {
	m := NewManager(nil)

	if err := m.RecreateRender(7, func(*Render, *Session) error { return nil }); !errors.Is(err, ErrSessionGone) {
		t.Fatalf("expected ErrSessionGone, got %v", err)
	}

	clock := time.Unix(1700000000, 0)
	mClock := NewManager(func() time.Time { return clock })
	mClock.Replace(7, ScreenState{Kind: ScreenDashboard})

	if err := mClock.RecreateRender(7, func(*Render, *Session) error { return nil }); err != nil {
		t.Fatalf("live session must re-create, got %v", err)
	}

	clock = clock.Add(SessionTTL + time.Second)
	if err := mClock.RecreateRender(7, func(*Render, *Session) error { return nil }); !errors.Is(err, ErrSessionExpired) {
		t.Fatalf("expected ErrSessionExpired, got %v", err)
	}

	mClock.Close(7)
	if err := mClock.RecreateRender(7, func(*Render, *Session) error { return nil }); !errors.Is(err, ErrSessionGone) {
		t.Fatalf("closed session must report gone, got %v", err)
	}

	// Detached (replaced, not closed) also refuses: the replacement is live.
	mClock.Replace(8, ScreenState{Kind: ScreenDashboard})
	mClock.Replace(8, ScreenState{Kind: ScreenDashboard})
	if err := mClock.RecreateRender(8, func(*Render, *Session) error { return nil }); err != nil {
		t.Fatalf("the replacement is live and must re-create, got %v", err)
	}
}

func TestRecreateRenderSuppressedWhilePendingInput(t *testing.T) {
	m := NewManager(nil)
	session := m.Replace(7, ScreenState{Kind: ScreenDashboard})

	// No pending: proceeds.
	if err := m.RecreateRender(7, func(*Render, *Session) error { return nil }); err != nil {
		t.Fatalf("expected re-creation without pending, got %v", err)
	}

	// Live pending: suppressed, reported as ErrRecreateSuppressed.
	if _, err := m.Apply(7, 0, func(s *Session) error {
		s.Pending = &Pending{ID: s.NextPendingID(), Kind: PendingStringInput, ExpiresAt: m.Now().Add(PendingTTL)}

		return nil
	}); err != nil {
		t.Fatalf("set pending: %v", err)
	}
	calls := 0
	if err := m.RecreateRender(7, func(*Render, *Session) error {
		calls++

		return nil
	}); !errors.Is(err, ErrRecreateSuppressed) {
		t.Fatalf("suppression must report ErrRecreateSuppressed so the caller keeps its schedule, got %v", err)
	}
	if calls != 0 {
		t.Fatalf("expected send to be suppressed while an input is pending, got %d calls", calls)
	}

	// Expired-but-uncleared pendings still suppress (conservative, matching
	// the pending-matching stance).
	if _, err := m.Apply(7, 0, func(s *Session) error {
		s.Pending.ExpiresAt = m.Now().Add(-time.Second)

		return nil
	}); err != nil {
		t.Fatalf("expire pending: %v", err)
	}
	if !session.HasPendingInput() {
		t.Fatal("expired pending must still count as pending input")
	}
	calls = 0
	if err := m.RecreateRender(7, func(*Render, *Session) error {
		calls++

		return nil
	}); !errors.Is(err, ErrRecreateSuppressed) {
		t.Fatalf("suppression must report ErrRecreateSuppressed so the caller keeps its schedule, got %v", err)
	}
	if calls != 0 {
		t.Fatalf("expected expired pending to still suppress, got %d calls", calls)
	}
}

func TestRecreateRenderSendFailureKeepsGatePosition(t *testing.T) {
	m := NewManager(nil)
	session := m.Replace(7, ScreenState{Kind: ScreenDashboard})
	_, _, _, epoch := session.Snapshot()

	boom := errors.New("send failed")
	if err := m.RecreateRender(7, func(*Render, *Session) error { return boom }); !errors.Is(err, boom) {
		t.Fatalf("send failure must propagate, got %v", err)
	}

	// The failed re-creation's epoch must not block a later render.
	applied := false
	if err := m.ApplyRender(renderOf(session, true), session, func() error {
		applied = true

		return nil
	}); err != nil {
		t.Fatalf("ApplyRender: %v", err)
	}
	_ = epoch
	if !applied {
		t.Fatal("a failed re-creation must not advance the render gate")
	}
}

func sessionExpiresAt(_ *Manager, session *Session) time.Time {
	session.mu.Lock()
	defer session.mu.Unlock()

	return session.ExpiresAt
}

// TestRecreateRenderEpochsUniqueUnderConcurrency pins the shared-epoch fix:
// re-creation mints render epochs from the manager-wide counter, which is
// shared across admins — one admin's re-creation runs concurrently with
// another admin's applies (per-session locks do not order them), so the mint
// must hold the manager lock or increments are lost and two renders share an
// epoch, making ApplyRender silently discard the later one through its
// lastApplied check.
func TestRecreateRenderEpochsUniqueUnderConcurrency(t *testing.T) {
	m := NewManager(nil)
	m.Replace(7, ScreenState{Kind: ScreenDashboard})
	m.Replace(8, ScreenState{Kind: ScreenDashboard})

	const recreators = 4
	const appliers = 8
	const iterations = 300

	var mu sync.Mutex
	epochs := make([]uint64, 0, (recreators+appliers)*iterations)
	record := func(epoch uint64) {
		mu.Lock()
		epochs = append(epochs, epoch)
		mu.Unlock()
	}

	var wg sync.WaitGroup
	for range recreators {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range iterations {
				var epoch uint64
				if err := m.RecreateRender(7, func(render *Render, _ *Session) error {
					epoch = render.Epoch

					return nil
				}); err != nil {
					t.Errorf("RecreateRender: %v", err)

					return
				}
				record(epoch)
			}
		}()
	}
	for range appliers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range iterations {
				// A DIFFERENT admin: the per-session lock does not order this
				// against admin 7's re-creations — only the manager lock can.
				result, err := m.Apply(8, 0, func(s *Session) error {
					s.MarkRerender()

					return nil
				})
				if err != nil {
					t.Errorf("Apply: %v", err)

					return
				}
				record(result.Render.Epoch)
			}
		}()
	}
	wg.Wait()

	seen := make(map[uint64]struct{}, len(epochs))
	for _, epoch := range epochs {
		if _, dup := seen[epoch]; dup {
			t.Fatalf("duplicate epoch %d minted across %d concurrent allocations (lost increment)", epoch, len(epochs))
		}
		seen[epoch] = struct{}{}
	}
}

// TestRecreateRenderRefusesReplacedSessionEpochMint pins the ownership fix:
// Replace publishes the replacement session (map write + epoch mint under the
// manager lock) BEFORE it re-acquires the old session's lock to mark it
// detached, so an in-flight re-creation can pass its detached check, watch
// the replacement appear in the map, and then mint a NEWER epoch for the old
// session. Once the re-creation's send completes, gate.lastApplied sits past
// the replacement's epoch and ApplyRender silently drops the replacement's
// initial render while the re-created message's adoption onto the old
// session is refused — the reopened menu stays buried. The clock read of the
// expiry check is the exact window between the detached check and the mint
// (the old session's lock is held there, which is precisely why Replace's
// detach step is still pending), so the staged clock performs the
// replacement's publish — without the detach — exactly there.
func TestRecreateRenderRefusesReplacedSessionEpochMint(t *testing.T) {
	var staged bool
	m := NewManager(func() time.Time { return time.Now().UTC() })

	m.Replace(7, ScreenState{Kind: ScreenDashboard})
	replaced := m.Session(7)

	m.now = func() time.Time {
		if staged {
			return time.Now().UTC()
		}
		staged = true

		// The concurrent command's replacement publish: a fresh session with
		// a minted epoch becomes the chat's session. No detach — Replace
		// detaches only after re-acquiring the old session's lock, which the
		// in-flight re-creation still holds.
		m.mu.Lock()
		fresh := &Session{
			AdminID:   7,
			Seq:       randomSeq(),
			Screen:    ScreenState{Kind: ScreenDashboard},
			ExpiresAt: time.Now().UTC().Add(SessionTTL),
		}
		fresh.Epoch = m.nextEpochLocked()
		m.sessions[7] = fresh
		m.mu.Unlock()

		return time.Now().UTC()
	}

	sent := false
	err := m.RecreateRender(7, func(_ *Render, _ *Session) error {
		sent = true

		return nil
	})
	if !errors.Is(err, ErrDetached) {
		t.Fatalf("a session replaced between lookup and mint must refuse the re-creation, got %v", err)
	}
	if sent {
		t.Fatal("no re-creation may run for a replaced session")
	}

	// Complete the replacement the way Replace does after the publish:
	// re-acquiring the old session's lock (free by now) to mark it detached.
	replaced.mu.Lock()
	replaced.detached = true
	replaced.mu.Unlock()

	// The replacement's initial render must go through: minting for the old
	// session would push lastApplied past the replacement's epoch and drop
	// this render silently — the reopened menu would stay buried.
	fresh := m.Session(7)
	if fresh == replaced {
		t.Fatal("test staging: the replacement must be a different session")
	}
	fresh.mu.Lock()
	epoch := fresh.Epoch
	seq := fresh.Seq
	screen := fresh.Screen
	fresh.mu.Unlock()

	applied := false
	if err := m.ApplyRender(&Render{AdminID: 7, Epoch: epoch, Seq: seq, Screen: screen}, fresh, func() error {
		applied = true

		return nil
	}); err != nil {
		t.Fatalf("ApplyRender: %v", err)
	}
	if !applied {
		t.Fatal("the replacement's initial render must not be dropped by the interrupted re-creation")
	}
}
