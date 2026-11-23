package tooluse

import (
	"sync"
	"time"
)

// chatCooldownMaxEntries soft-caps a chatCooldown's map.
const chatCooldownMaxEntries = 4096

// chatCooldown spaces side-effecting sends per chat. It is deliberately not a
// general rate limiter: one map of last-dispatch times, one mutex. Admission
// is an atomic check-and-reserve, so concurrent turns in one chat cannot both
// pass. Each tool owns its own instance, so cooldowns never interact.
type chatCooldown struct {
	mu   sync.Mutex
	last map[int64]time.Time
}

// admit checks and, when admitted, reserves the chat's slot. A send inside the
// window is declined only when enforce is set (nothing is stamped then); an
// unenforced send always passes. Every admitted send stamps the chat, so an
// enforced send cannot follow right behind an unenforced one.
func (l *chatCooldown) admit(chatID int64, enforce bool, now time.Time, cooldown time.Duration) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if enforce {
		if at, ok := l.last[chatID]; ok {
			if wait := cooldown - now.Sub(at); wait > 0 {
				return false, wait
			}
		}
	}
	l.stamp(chatID, now, cooldown)

	return true, 0
}

// cooling reports, without reserving, whether an enforced send in the chat
// would currently be declined. It only decides advertisement; the dispatch
// check in admit stays authoritative.
func (l *chatCooldown) cooling(chatID int64, now time.Time, cooldown time.Duration) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	at, ok := l.last[chatID]

	return ok && now.Sub(at) < cooldown
}

// release undoes an admit that was followed by no real send. admit only
// succeeds outside the window, so dropping the entry restores the prior state.
func (l *chatCooldown) release(chatID int64) {
	l.mu.Lock()
	defer l.mu.Unlock()

	delete(l.last, chatID)
}

func (l *chatCooldown) stamp(chatID int64, now time.Time, cooldown time.Duration) {
	if l.last == nil {
		l.last = make(map[int64]time.Time)
	}
	if _, known := l.last[chatID]; !known && len(l.last) >= chatCooldownMaxEntries {
		var oldestChat int64
		var oldest time.Time
		first := true
		for chat, at := range l.last {
			if now.Sub(at) >= cooldown {
				delete(l.last, chat) // expired entries carry no information

				continue
			}
			if first || at.Before(oldest) {
				oldestChat, oldest, first = chat, at, false
			}
		}
		if len(l.last) >= chatCooldownMaxEntries && !first {
			delete(l.last, oldestChat)
		}
	}
	l.last[chatID] = now
}
