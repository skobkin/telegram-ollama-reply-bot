package bot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"telegram-ollama-reply-bot/internal/adminconfig"
	"telegram-ollama-reply-bot/internal/config"
	"telegram-ollama-reply-bot/internal/content/extractor"
	"telegram-ollama-reply-bot/internal/llm"
	"telegram-ollama-reply-bot/internal/state"
	"telegram-ollama-reply-bot/internal/support/markdown"

	tg "github.com/mymmrac/telego"
)

// --- decision function ---

func TestAutoSummaryTargetURLs(t *testing.T) {
	longLink := "https://example.com/article"
	otherLink := "https://example.org/other"

	tests := []struct {
		name           string
		message        tg.Message
		enabled        bool
		conversational bool
		maxLinks       int
		want           []string
	}{
		{
			name:    "disabled",
			message: groupMessage("read " + longLink),
			want:    nil,
		},
		{
			name:           "conversational trigger",
			message:        groupMessage("read " + longLink),
			enabled:        true,
			conversational: true,
			want:           nil,
		},
		{
			name: "bot author",
			message: func() tg.Message {
				m := groupMessage("read " + longLink)
				m.From.IsBot = true

				return m
			}(),
			enabled: true,
			want:    nil,
		},
		{
			name:    "command message",
			message: groupMessage("/summarize " + longLink),
			enabled: true,
			want:    nil,
		},
		{
			name:     "plain link",
			message:  groupMessage("read this " + longLink),
			enabled:  true,
			maxLinks: 3,
			want:     []string{longLink},
		},
		{
			name: "caption with entity link",
			message: func() tg.Message {
				m := groupMessage("")
				m.Caption = "look at this"
				m.CaptionEntities = []tg.MessageEntity{{
					Type:   tg.EntityTypeTextLink,
					Offset: 0,
					Length: len([]rune("look")),
					URL:    longLink,
				}}

				return m
			}(),
			enabled:  true,
			maxLinks: 3,
			want:     []string{longLink},
		},
		{
			name:     "duplicates deduplicated",
			message:  groupMessage(longLink + " and again " + longLink),
			enabled:  true,
			maxLinks: 3,
			want:     []string{longLink},
		},
		{
			name:     "capped at max links",
			message:  groupMessage(fmt.Sprintf("%s %s %s https://example.net/third", longLink, otherLink, "https://example.net/x")),
			enabled:  true,
			maxLinks: 2,
			want:     []string{longLink, otherLink},
		},
		{
			name:     "no links",
			message:  groupMessage("just chatting"),
			enabled:  true,
			maxLinks: 3,
			want:     nil,
		},
		{
			name:     "non-http scheme ignored",
			message:  groupMessage("ftp://example.com/file"),
			enabled:  true,
			maxLinks: 3,
			want:     nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := autoSummaryTargetURLs(tt.message, tt.enabled, tt.conversational, tt.maxLinks)
			if len(got) != len(tt.want) {
				t.Fatalf("expected %v, got %v", tt.want, got)
			}
			for i := range tt.want {
				if got[i] != tt.want[i] {
					t.Fatalf("expected %v, got %v", tt.want, got)
				}
			}
		})
	}
}

func groupMessage(text string) tg.Message {
	return tg.Message{
		MessageID: 12,
		Chat:      tg.Chat{ID: -100999, Type: tg.ChatTypeSupergroup, Title: "Test Group"},
		Text:      text,
		From:      &tg.User{ID: 2},
	}
}

// --- tracker ---

func TestAutoSummaryTrackerReserveAndCooldown(t *testing.T) {
	tracker := newAutoSummaryTracker(4)
	now := time.Now()

	if !tracker.reserve(1, "https://a.example/x", now, time.Hour) {
		t.Fatalf("first reserve must succeed")
	}
	if tracker.reserve(1, "https://a.example/x", now.Add(time.Minute), time.Hour) {
		t.Fatalf("reserve within cooldown must fail")
	}
	if !tracker.reserve(1, "https://a.example/x", now.Add(2*time.Hour), time.Hour) {
		t.Fatalf("reserve after cooldown expiry must succeed")
	}
}

func TestAutoSummaryTrackerReleaseAllowsRetry(t *testing.T) {
	tracker := newAutoSummaryTracker(4)
	now := time.Now()

	if !tracker.reserve(1, "https://a.example/x", now, time.Hour) {
		t.Fatalf("first reserve must succeed")
	}
	tracker.release(1, "https://a.example/x")
	if !tracker.reserve(1, "https://a.example/x", now.Add(time.Minute), time.Hour) {
		t.Fatalf("reserve after release must succeed")
	}
}

func TestAutoSummaryTrackerPerChatIsolation(t *testing.T) {
	tracker := newAutoSummaryTracker(4)
	now := time.Now()

	if !tracker.reserve(1, "https://a.example/x", now, time.Hour) {
		t.Fatalf("reserve in chat 1 must succeed")
	}
	if !tracker.reserve(2, "https://a.example/x", now, time.Hour) {
		t.Fatalf("same URL in chat 2 must not be on chat 1 cooldown")
	}
}

func TestAutoSummaryTrackerEvictsOldest(t *testing.T) {
	tracker := newAutoSummaryTracker(2)
	base := time.Now()

	if !tracker.reserve(1, "https://a.example/1", base, time.Hour) {
		t.Fatalf("reserve 1")
	}
	if !tracker.reserve(1, "https://a.example/2", base.Add(time.Minute), time.Hour) {
		t.Fatalf("reserve 2")
	}
	if !tracker.reserve(1, "https://a.example/3", base.Add(2*time.Minute), time.Hour) {
		t.Fatalf("reserve 3 must evict the oldest entry")
	}

	// Oldest URL (first) was evicted: it can be reserved again.
	if !tracker.reserve(1, "https://a.example/1", base.Add(2*time.Minute), time.Hour) {
		t.Fatalf("evicted URL must be reservable again")
	}
	// Newest URL must still be on cooldown.
	if tracker.reserve(1, "https://a.example/3", base.Add(2*time.Minute), time.Hour) {
		t.Fatalf("recent URL must stay on cooldown after unrelated eviction")
	}
}

func TestAutoSummaryTrackerSingleFlight(t *testing.T) {
	tracker := newAutoSummaryTracker(4)

	if !tracker.beginChat(1) {
		t.Fatalf("first beginChat must succeed")
	}
	if tracker.beginChat(1) {
		t.Fatalf("second beginChat while running must fail")
	}
	if !tracker.beginChat(2) {
		t.Fatalf("beginChat for another chat must succeed")
	}
	tracker.endChat(1)
	if !tracker.beginChat(1) {
		t.Fatalf("beginChat after endChat must succeed")
	}
}

// --- pipeline tests ---

// stubExtractor implements extractor.Extractor, recording call counts.
type stubExtractor struct {
	mu      sync.Mutex
	calls   int
	article extractor.Article
	err     error
}

func (s *stubExtractor) GetArticleFromURL(context.Context, string) (extractor.Article, error) {
	s.mu.Lock()
	s.calls++
	article, err := s.article, s.err
	s.mu.Unlock()

	return article, err
}

func (s *stubExtractor) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.calls
}

// stubSummarizer implements llmSummarizer.
type stubSummarizer struct {
	mu    sync.Mutex
	calls int
	reply string
}

func (s *stubSummarizer) Summarize(context.Context, llm.PromptScope, string, string) (string, *llm.TokenUsage, error) {
	s.mu.Lock()
	s.calls++
	reply := s.reply
	s.mu.Unlock()

	return reply, &llm.TokenUsage{}, nil
}

func (s *stubSummarizer) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.calls
}

// autoSummaryHistorySpy records appended messages with their text.
type autoSummaryHistorySpy struct {
	mu       sync.Mutex
	messages []state.Message
}

func (s *autoSummaryHistorySpy) AppendMessage(_ state.ConversationScope, msg state.Message) {
	s.mu.Lock()
	s.messages = append(s.messages, msg)
	s.mu.Unlock()
}

func (s *autoSummaryHistorySpy) Snapshot(state.ConversationScope) state.ConversationSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()

	return state.ConversationSnapshot{Messages: append([]state.Message(nil), s.messages...)}
}

func (s *autoSummaryHistorySpy) Search(state.ConversationScope, state.HistorySearchQuery) []state.HistorySearchMatch {
	return nil
}
func (s *autoSummaryHistorySpy) SetEarlierSummary(state.ConversationScope, string, int) {}
func (s *autoSummaryHistorySpy) SetMessageReactions(int64, int, int64, []state.Reaction) bool {
	return false
}
func (s *autoSummaryHistorySpy) Reset(state.ConversationScope) {}
func (s *autoSummaryHistorySpy) ResetChat(int64)               {}

func (s *autoSummaryHistorySpy) botReplies() []state.Message {
	s.mu.Lock()
	defer s.mu.Unlock()

	var replies []state.Message
	for _, msg := range s.messages {
		if msg.IsMe {
			replies = append(replies, msg)
		}
	}

	return replies
}

// autoSummaryConfig builds the operational config used by the pipeline tests.
func autoSummaryConfig() config.BotConfig {
	return config.BotConfig{
		AdminIDs:          []int64{7},
		ProcessingTimeout: 5 * time.Second,
		AutoSummary: config.AutoSummaryConfig{
			MinTextLength:      100,
			URLCooldown:        time.Hour,
			MaxLinksPerMessage: 3,
		},
	}
}

// autoSummaryAdminStore wraps adminStoreStub for the auto-summary pipeline
// tests: a mentions-only interactivity mode (ordinary group messages are
// ignored conversationally but reach the auto-summary branch) and an
// allow_all access policy so the access gate passes group messages through.
type autoSummaryAdminStore struct {
	adminStoreStub
}

func (s *autoSummaryAdminStore) GetAccessPolicy(context.Context) (adminconfig.AccessPolicy, error) {
	return adminconfig.AccessPolicyAllowAll, nil
}

func newAutoSummaryAdminStore(enabled bool) *autoSummaryAdminStore {
	global := adminconfig.GlobalSettings{
		DefaultInteractivityMode:  adminconfig.InteractivityMentionsOnly,
		DefaultAutoSummaryEnabled: enabled,
	}

	return &autoSummaryAdminStore{adminStoreStub{GlobalSettings: global}}
}

// newAutoSummaryPipelineBot builds a pipeline test bot with the
// auto-summarizer stubs wired in.
func newAutoSummaryPipelineBot(t *testing.T, extractorStub *stubExtractor, summarizer *stubSummarizer, history *autoSummaryHistorySpy) *pipelineTestBot {
	t.Helper()

	p := newPipelineTestBot(t, autoSummaryConfig(), newAutoSummaryAdminStore(true))
	p.bot.extractor = extractorStub
	p.bot.llm = summarizer
	p.bot.sanitizer = markdown.NewTgMarkdownV2Sanitizer()
	p.bot.history = history
	p.bot.autoSummaryURLs = newAutoSummaryTracker(autoSummaryMaxTrackedURLsPerChat)

	return p
}

// waitFor polls until the condition holds or the deadline passes.
func waitFor(t *testing.T, condition func() bool) {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("condition not met within timeout")
}

// richTexts decodes the rich message markdown of every captured
// sendRichMessage.
func (f *fakeAPIServer) richTexts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	var texts []string
	for _, request := range f.requests {
		if request.Method != "sendRichMessage" {
			continue
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(request.Body), &payload); err != nil {
			continue
		}
		rich, _ := payload["rich_message"].(map[string]any)
		text, _ := rich["markdown"].(string)
		texts = append(texts, text)
	}

	return texts
}

func TestAutoSummaryPipelinePostsSummaryAndRecordsHistory(t *testing.T) {
	extractorStub := &stubExtractor{article: extractor.Article{
		Title:    "Long Article",
		Text:     strings.Repeat("word ", 300),
		Markdown: "# Long Article\n\n" + strings.Repeat("word ", 300),
		URL:      "https://example.com/article",
	}}
	summarizer := &stubSummarizer{reply: "Auto summary of the article"}
	history := &autoSummaryHistorySpy{}
	p := newAutoSummaryPipelineBot(t, extractorStub, summarizer, history)

	p.deliver(t, pipelineGroupUpdate(2, "check this https://example.com/article"))

	waitFor(t, func() bool { return len(p.api.richTexts()) == 1 })

	texts := p.api.richTexts()
	if !strings.Contains(texts[0], "Auto summary of the article") || !strings.Contains(texts[0], "[src](https://example.com/article)") {
		t.Fatalf("unexpected rich message: %q", texts[0])
	}

	waitFor(t, func() bool { return len(history.botReplies()) == 1 })
	reply := history.botReplies()[0]
	if !reply.IsMe || !strings.Contains(reply.Text, "[src](https://example.com/article)") {
		t.Fatalf("unexpected history reply: %+v", reply)
	}
	if extractorStub.callCount() != 1 || summarizer.callCount() != 1 {
		t.Fatalf("unexpected call counts: extractor=%d summarizer=%d", extractorStub.callCount(), summarizer.callCount())
	}
}

func TestAutoSummaryPipelineDisabledDoesNothing(t *testing.T) {
	extractorStub := &stubExtractor{article: extractor.Article{Text: strings.Repeat("word ", 300), URL: "https://example.com/article"}}
	summarizer := &stubSummarizer{reply: "summary"}
	history := &autoSummaryHistorySpy{}
	p := newAutoSummaryPipelineBot(t, extractorStub, summarizer, history)

	// Same pipeline but the admin store leaves the feature off.
	p.bot.admin = adminconfig.NewService(newAutoSummaryAdminStore(false), nil, nil)

	p.deliver(t, pipelineGroupUpdate(2, "check this https://example.com/article"))

	time.Sleep(200 * time.Millisecond)
	if extractorStub.callCount() != 0 || summarizer.callCount() != 0 {
		t.Fatalf("disabled mode must not fetch or summarize: extractor=%d summarizer=%d", extractorStub.callCount(), summarizer.callCount())
	}
	if len(p.api.richTexts()) != 0 || len(history.botReplies()) != 0 {
		t.Fatalf("disabled mode must not post anything")
	}
}

func TestAutoSummaryPipelineShortContentKeepsCooldown(t *testing.T) {
	extractorStub := &stubExtractor{article: extractor.Article{Text: "short", URL: "https://example.com/short"}}
	summarizer := &stubSummarizer{reply: "summary"}
	history := &autoSummaryHistorySpy{}
	p := newAutoSummaryPipelineBot(t, extractorStub, summarizer, history)

	linkMessage := pipelineGroupUpdate(2, "check this https://example.com/short")
	p.deliver(t, linkMessage)

	// First delivery: fetched once, nothing sent (too short), cooldown kept.
	waitFor(t, func() bool { return extractorStub.callCount() == 1 })
	if len(p.api.richTexts()) != 0 {
		t.Fatalf("short content must not be summarized")
	}

	// Repost of the same link: still on cooldown, no second fetch.
	p.deliver(t, linkMessage)
	time.Sleep(200 * time.Millisecond)
	if extractorStub.callCount() != 1 {
		t.Fatalf("short-content link must stay on cooldown, fetches=%d", extractorStub.callCount())
	}
}

func TestAutoSummaryPipelineFailureReleasesCooldown(t *testing.T) {
	extractorStub := &stubExtractor{err: errors.New("boom")}
	summarizer := &stubSummarizer{reply: "summary"}
	history := &autoSummaryHistorySpy{}
	p := newAutoSummaryPipelineBot(t, extractorStub, summarizer, history)

	linkMessage := pipelineGroupUpdate(2, "check this https://example.com/fail")
	p.deliver(t, linkMessage)
	waitFor(t, func() bool { return extractorStub.callCount() == 1 })
	if len(p.api.richTexts()) != 0 {
		t.Fatalf("failed extraction must not post anything")
	}

	// Failure releases the reservation: a repost retries the fetch.
	p.deliver(t, linkMessage)
	waitFor(t, func() bool { return extractorStub.callCount() == 2 })
}

func TestAutoSummaryPipelineRepostAfterSuccessFetchesOnce(t *testing.T) {
	extractorStub := &stubExtractor{article: extractor.Article{
		Text: strings.Repeat("word ", 300),
		URL:  "https://example.com/article",
	}}
	summarizer := &stubSummarizer{reply: "Auto summary"}
	history := &autoSummaryHistorySpy{}
	p := newAutoSummaryPipelineBot(t, extractorStub, summarizer, history)

	linkMessage := pipelineGroupUpdate(2, "check this https://example.com/article")
	p.deliver(t, linkMessage)
	waitFor(t, func() bool { return extractorStub.callCount() == 1 })
	waitFor(t, func() bool { return len(p.api.richTexts()) == 1 })

	// The same URL reposted: cooldown suppresses a second fetch.
	p.deliver(t, linkMessage)
	time.Sleep(200 * time.Millisecond)
	if extractorStub.callCount() != 1 || len(p.api.richTexts()) != 1 {
		t.Fatalf("repost must not re-fetch: extractor=%d sends=%d", extractorStub.callCount(), len(p.api.richTexts()))
	}
}

func TestAutoSummaryPipelineCapsLinksPerMessage(t *testing.T) {
	article := extractor.Article{Text: strings.Repeat("word ", 300)}
	extractorStub := &stubExtractor{article: article}
	summarizer := &stubSummarizer{reply: "Auto summary"}
	history := &autoSummaryHistorySpy{}
	p := newAutoSummaryPipelineBot(t, extractorStub, summarizer, history)

	p.deliver(t, pipelineGroupUpdate(2,
		"https://example.com/one https://example.com/two https://example.com/three https://example.com/four"))

	waitFor(t, func() bool { return extractorStub.callCount() == 3 })
	if extractorStub.callCount() > 3 {
		t.Fatalf("link cap exceeded: %d fetches", extractorStub.callCount())
	}
}

func TestAutoSummaryPipelineConversationalMessageNotAutoSummarized(t *testing.T) {
	extractorStub := &stubExtractor{article: extractor.Article{Text: strings.Repeat("word ", 300), URL: "https://example.com/article"}}
	summarizer := &stubSummarizer{reply: "Auto summary"}
	history := &autoSummaryHistorySpy{}
	p := newAutoSummaryPipelineBot(t, extractorStub, summarizer, history)

	// A private DM is always conversational (the bot would answer it), so
	// the auto-summarizer must skip it. Interactivity "disabled" keeps the
	// non-admin DM out of processMention too, letting this test isolate the
	// auto-summary skip without driving the full reply path.
	store := newAutoSummaryAdminStore(true)
	store.DefaultInteractivityMode = adminconfig.InteractivityDisabled
	p.bot.admin = adminconfig.NewService(store, nil, nil)

	p.deliver(t, pipelinePrivateUpdate(2, "check this https://example.com/article"))

	time.Sleep(300 * time.Millisecond)
	if extractorStub.callCount() != 0 {
		t.Fatalf("conversational message must not be auto-summarized, fetches=%d", extractorStub.callCount())
	}
	if len(p.api.richTexts()) != 0 {
		t.Fatalf("conversational message must not be answered, sends=%d", len(p.api.richTexts()))
	}
}
