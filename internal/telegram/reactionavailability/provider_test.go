package reactionavailability

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	t "github.com/mymmrac/telego"
	"telegram-ollama-reply-bot/internal/telegram/reactionemoji"
)

type getterFunc func(context.Context, *t.GetChatParams) (*t.ChatFullInfo, error)

func (f getterFunc) GetChat(ctx context.Context, p *t.GetChatParams) (*t.ChatFullInfo, error) {
	return f(ctx, p)
}

func TestTelegramPresenceAndCandidateIntersection(tst *testing.T) {
	for _, tc := range []struct {
		body string
		want []string
	}{
		{`{"id":1,"title":"Test"}`, reactionemoji.ReactionCandidates()},
		{`{"id":1,"available_reactions":[]}`, []string{}},
		{`{"id":1,"available_reactions":[{"type":"emoji","emoji":"❤"},{"type":"emoji","emoji":"👍"},{"type":"emoji","emoji":"😂"},{"type":"custom_emoji","custom_emoji_id":"123"},{"type":"paid"}]}`, []string{"👍", "❤"}},
	} {
		var chat t.ChatFullInfo
		if err := json.Unmarshal([]byte(tc.body), &chat); err != nil {
			tst.Fatal(err)
		}
		p := New(getterFunc(func(context.Context, *t.GetChatParams) (*t.ChatFullInfo, error) { return &chat, nil }), nil)
		got := p.Lookup(context.Background(), 1, "Test")
		if !got.Known || got.Stale || !slices.Equal(got.Candidates, tc.want) {
			tst.Fatalf("body=%s got=%+v", tc.body, got)
		}
	}
}

func TestTTLFailuresAndEmptyWarningSuppression(tst *testing.T) {
	var logs bytes.Buffer
	now := time.Unix(100, 0)
	count := 0
	failed := false
	empty := false
	p := New(getterFunc(func(context.Context, *t.GetChatParams) (*t.ChatFullInfo, error) {
		count++
		if failed {
			return nil, errors.New("secret URL")
		}
		reactions := []t.ReactionType{}
		if !empty {
			reactions = append(reactions, &t.ReactionTypeEmoji{Emoji: "👍"})
		}
		return &t.ChatFullInfo{Title: "Test title", AvailableReactions: reactions}, nil
	}), slog.New(slog.NewTextHandler(&logs, nil)))
	p.now = func() time.Time { return now }
	first := p.Lookup(context.Background(), 42, "")
	first.Candidates[0] = "mutated"
	if got := p.Lookup(context.Background(), 42, ""); got.Candidates[0] != "👍" || count != 1 {
		tst.Fatal(got, count)
	}
	now = now.Add(DefaultTTL)
	failed = true
	got := p.Lookup(context.Background(), 42, "")
	if !got.Known || !got.Stale || !slices.Equal(got.Candidates, []string{"👍"}) {
		tst.Fatal(got)
	}
	p.Lookup(context.Background(), 42, "")
	p.Peek(42)
	if count != 2 {
		tst.Fatal("failed lookup not backed off", count)
	}
	now = now.Add(failureBackoff)
	failed = false
	empty = true
	got = p.Lookup(context.Background(), 42, "")
	if !got.Known || len(got.Candidates) != 0 {
		tst.Fatal(got)
	}
	now = now.Add(DefaultTTL)
	p.Lookup(context.Background(), 42, "")
	if strings.Count(logs.String(), "no supported outgoing reactions available") != 1 || !strings.Contains(logs.String(), "chat_id=42") || !strings.Contains(logs.String(), "Test title") || strings.Contains(logs.String(), "secret URL") {
		tst.Fatal(logs.String())
	}
}

func TestColdLookupFailureUsesUnverifiedWhitelist(tst *testing.T) {
	p := New(getterFunc(func(context.Context, *t.GetChatParams) (*t.ChatFullInfo, error) { return nil, errors.New("offline") }), nil)
	got := p.Lookup(context.Background(), 1, "")
	if got.Known || !slices.Equal(got.Candidates, reactionemoji.ReactionCandidates()) {
		tst.Fatal(got)
	}
}

func TestConcurrentLookupCancellationAndInvalidation(tst *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	p := New(getterFunc(func(context.Context, *t.GetChatParams) (*t.ChatFullInfo, error) {
		if calls.Add(1) == 1 {
			close(started)
		}
		<-release
		return &t.ChatFullInfo{Title: "Test"}, nil
	}), nil)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); p.Lookup(context.Background(), 1, "") }()
	<-started
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := p.Lookup(ctx, 1, ""); got.Known {
		tst.Fatal(got)
	}
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); p.Lookup(context.Background(), 1, "") }()
	}
	close(release)
	wg.Wait()
	if calls.Load() != 1 {
		tst.Fatal("redundant refresh", calls.Load())
	}
	p.Invalidate(1)
	if got := p.Peek(1); !got.Stale {
		tst.Fatal(got)
	}
	p.Lookup(context.Background(), 1, "")
	if calls.Load() != 2 {
		tst.Fatal("invalidation not refreshed")
	}
}

func TestInvalidationWinsOverInflightResponse(tst *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})
	p := New(getterFunc(func(context.Context, *t.GetChatParams) (*t.ChatFullInfo, error) {
		close(started)
		<-release
		return &t.ChatFullInfo{}, nil
	}), nil)
	go func() { p.Lookup(context.Background(), 1, ""); close(done) }()
	<-started
	p.Invalidate(1)
	close(release)
	<-done
	if p.Peek(1).Known {
		tst.Fatal("stale response committed after invalidation")
	}
}

func TestCacheCapacityIncludesActiveRefreshes(tst *testing.T) {
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	var wg sync.WaitGroup
	p := New(getterFunc(func(context.Context, *t.GetChatParams) (*t.ChatFullInfo, error) {
		started <- struct{}{}
		<-release
		return &t.ChatFullInfo{}, nil
	}), nil)
	p.capacity = 2
	for _, id := range []int64{1, 2} {
		wg.Add(1)
		go func() { defer wg.Done(); p.Lookup(context.Background(), id, "") }()
	}
	<-started
	<-started
	if got := p.Lookup(context.Background(), 3, ""); got.Known {
		tst.Fatal(got)
	}
	p.mu.Lock()
	size := len(p.entries)
	p.mu.Unlock()
	if size != 2 {
		tst.Fatal(size)
	}
	close(release)
	wg.Wait()
	// An idle entry can now be evicted; the cap still holds.
	p.getter = getterFunc(func(context.Context, *t.GetChatParams) (*t.ChatFullInfo, error) { return &t.ChatFullInfo{}, nil })
	p.Lookup(context.Background(), 3, "")
	if len(p.entries) != 2 {
		tst.Fatal(len(p.entries))
	}
}
