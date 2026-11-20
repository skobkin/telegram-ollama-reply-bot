// Package reactionavailability caches Telegram's ordinary reaction candidates.
// It owns no conversational state and can be reused by other emoji selectors.
package reactionavailability

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"sync"
	"time"

	"telegram-ollama-reply-bot/internal/telegram/reactionemoji"

	t "github.com/mymmrac/telego"
	"github.com/mymmrac/telego/telegoapi"
	tu "github.com/mymmrac/telego/telegoutil"
)

const (
	DefaultTTL     = 30 * time.Minute
	maxEntries     = 4096
	lookupTimeout  = 5 * time.Second
	failureBackoff = time.Minute
)

type ChatGetter interface {
	GetChat(context.Context, *t.GetChatParams) (*t.ChatFullInfo, error)
}

// Snapshot distinguishes Telegram-confirmed availability from stale information
// and a cold failure's unverified whitelist. Candidates always belong to the
// outgoing whitelist, in canonical order; callers own the returned slice.
type Snapshot struct {
	Candidates []string
	Known      bool
	Stale      bool
	Title      string
}

type entry struct {
	candidates             []string
	known                  bool
	title                  string
	expires, retryAt, used time.Time
	loading                chan struct{}
	generation             uint64
	warnedEmpty            bool
}

type Provider struct {
	getter                  ChatGetter
	logger                  *slog.Logger
	mu                      sync.Mutex
	entries                 map[int64]*entry
	now                     func() time.Time
	ttl, timeLimit, backoff time.Duration
	capacity                int
}

func New(getter ChatGetter, logger *slog.Logger) *Provider {
	if logger == nil {
		logger = slog.Default()
	}
	return &Provider{getter: getter, logger: logger, entries: make(map[int64]*entry), now: time.Now,
		ttl: DefaultTTL, timeLimit: lookupTimeout, backoff: failureBackoff, capacity: maxEntries}
}

func snapshot(e *entry, now time.Time) Snapshot {
	if e == nil {
		return Snapshot{Candidates: reactionemoji.ReactionCandidates()}
	}
	candidates := e.candidates
	if !e.known {
		candidates = reactionemoji.ReactionCandidates()
	}
	return Snapshot{Candidates: slices.Clone(candidates), Known: e.known, Stale: e.known && !now.Before(e.expires), Title: e.title}
}

// Peek is strictly local: argument validation never issues Telegram requests.
func (p *Provider) Peek(chatID int64) Snapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	return snapshot(p.entries[chatID], p.now())
}

func (p *Provider) Invalidate(chatID int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e := p.entries[chatID]; e != nil {
		e.expires = time.Time{}
		e.retryAt = time.Time{}
		e.generation++
	}
}

// room evicts the least recently used idle entry. In-flight lookups count toward
// the cap and are never evicted, so neither deduplication nor memory bounds break.
func (p *Provider) room() bool {
	if len(p.entries) < p.capacity {
		return true
	}
	var oldest *entry
	var key int64
	for id, e := range p.entries {
		if e.loading == nil && (oldest == nil || e.used.Before(oldest.used)) {
			oldest = e
			key = id
		}
	}
	if oldest == nil {
		return false
	}
	delete(p.entries, key)
	return true
}

func (p *Provider) Lookup(ctx context.Context, chatID int64, title string) Snapshot {
	for {
		p.mu.Lock()
		now := p.now()
		e := p.entries[chatID]
		if e == nil {
			if !p.room() {
				p.mu.Unlock()
				return snapshot(nil, now)
			}
			e = &entry{title: title}
			p.entries[chatID] = e
		}
		e.used = now
		if e.title == "" {
			e.title = title
		}
		if (e.known && now.Before(e.expires)) || now.Before(e.retryAt) || ctx.Err() != nil || p.getter == nil {
			result := snapshot(e, now)
			p.mu.Unlock()
			return result
		}
		if done := e.loading; done != nil {
			p.mu.Unlock()
			select {
			case <-ctx.Done():
				return p.Peek(chatID)
			case <-done:
				continue
			}
		}
		done := make(chan struct{})
		e.loading = done
		generation := e.generation
		p.mu.Unlock()

		callCtx, cancel := context.WithTimeout(ctx, p.timeLimit)
		chat, err := p.getter.GetChat(callCtx, &t.GetChatParams{ChatID: tu.ID(chatID)})
		cancel()
		if chat == nil && err == nil {
			err = errors.New("empty getChat response")
		}

		p.mu.Lock()
		now = p.now()
		// Invalidation wins over a response from an earlier cache generation.
		if generation == e.generation {
			if err == nil {
				e.candidates = candidates(chat.AvailableReactions)
				e.known = true
				e.expires = now.Add(p.ttl)
				e.retryAt = time.Time{}
				if chat.Title != "" {
					e.title = chat.Title
				}
				if len(e.candidates) == 0 {
					if !e.warnedEmpty {
						p.logger.Warn("no supported outgoing reactions available", "chat_id", chatID, "chat_title", e.title)
						e.warnedEmpty = true
					}
				} else {
					e.warnedEmpty = false
				}
			} else if ctx.Err() == nil {
				e.retryAt = now.Add(p.backoff)
				class, code, delay := lookupFailure(err)
				if retry := now.Add(delay); retry.After(e.retryAt) {
					e.retryAt = retry
				}
				p.logger.Warn("reaction availability lookup failed", "chat_id", chatID, "error_class", class, "telegram_error_code", code)
			}
		}
		e.loading = nil
		close(done)
		result := snapshot(e, now)
		p.mu.Unlock()
		return result
	}
}

func candidates(available []t.ReactionType) []string {
	whitelist := reactionemoji.ReactionCandidates()
	if available == nil {
		return whitelist
	}
	allowed := make(map[string]bool, len(available))
	for _, item := range available {
		if emoji, ok := item.(*t.ReactionTypeEmoji); ok && emoji != nil {
			allowed[emoji.Emoji] = true
		}
	}
	result := make([]string, 0, len(whitelist))
	for _, emoji := range whitelist {
		if allowed[emoji] {
			result = append(result, emoji)
		}
	}
	return result
}

func lookupFailure(err error) (string, int, time.Duration) {
	var api *telegoapi.Error
	if errors.As(err, &api) {
		if api.ErrorCode == 429 {
			var delay time.Duration
			if api.Parameters != nil {
				delay = time.Duration(api.Parameters.RetryAfter) * time.Second
			}
			return "rate_limited", api.ErrorCode, delay
		}
		return "telegram_rejected", api.ErrorCode, 0
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return "cancelled", 0, 0
	}
	return "transport", 0, 0
}
