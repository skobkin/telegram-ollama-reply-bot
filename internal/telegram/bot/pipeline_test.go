package bot

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"telegram-ollama-reply-bot/internal/adminconfig"
	"telegram-ollama-reply-bot/internal/config"

	tg "github.com/mymmrac/telego"
	th "github.com/mymmrac/telego/telegohandler"
)

// fakeAPIRequest is one captured Bot API call.
type fakeAPIRequest struct {
	Method string
	Body   string
}

// fakeAPIServer records Bot API calls and answers with minimal valid
// responses (telego requires {"ok":true,"result":...}).
type fakeAPIServer struct {
	server   *httptest.Server
	mu       sync.Mutex
	requests []fakeAPIRequest
}

func newFakeAPIServer(t *testing.T) *fakeAPIServer {
	t.Helper()

	fake := &fakeAPIServer{}
	fake.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		fake.mu.Lock()
		fake.requests = append(fake.requests, fakeAPIRequest{
			Method: strings.TrimPrefix(r.URL.Path, "/bot1:"+strings.Repeat("A", 35)+"/"),
			Body:   string(body),
		})
		fake.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"result":{}}`))
	}))
	t.Cleanup(fake.server.Close)

	return fake
}

// callCount returns every captured request for a method, in order.
func (f *fakeAPIServer) callCount(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()

	count := 0
	for _, request := range f.requests {
		if request.Method == method {
			count++
		}
	}

	return count
}

// lastBodyText extracts the "text" field of the last captured sendMessage.
func (f *fakeAPIServer) lastText(method string) string {
	f.mu.Lock()
	defer f.mu.Unlock()

	for i := len(f.requests) - 1; i >= 0; i-- {
		if f.requests[i].Method != method {
			continue
		}

		var payload map[string]any
		if err := json.Unmarshal([]byte(f.requests[i].Body), &payload); err != nil {
			return ""
		}
		text, _ := payload["text"].(string)

		return text
	}

	return ""
}

// pipelineStatsSpy extends the DM-test stats spy with counters for the
// reset-specific metric and marks when API-calling handlers run.
type pipelineStatsSpy struct {
	dmStatsSpy
	chatResets atomic.Int32
}

func (s *pipelineStatsSpy) ChatHistoryReset() { s.chatResets.Add(1) }

// pipelineTestBot drives the REAL registerRoutes pipeline with spy stores and
// a fake Bot API, so handlers that answer messages run for real.
type pipelineTestBot struct {
	bot     *Bot
	bh      *th.BotHandler
	api     *fakeAPIServer
	history *dmHistorySpy
	stats   *pipelineStatsSpy
	catalog *dmCatalogSpy
}

func newPipelineTestBot(t *testing.T, cfg config.BotConfig, store adminconfig.Store) *pipelineTestBot {
	t.Helper()

	api := newFakeAPIServer(t)
	botAPI, err := tg.NewBot("1:"+strings.Repeat("A", 35), tg.WithAPIServer(api.server.URL), tg.WithDiscardLogger())
	if err != nil {
		t.Fatalf("NewBot: %v", err)
	}

	bh, err := th.NewBotHandler(botAPI, make(chan tg.Update))
	if err != nil {
		t.Fatalf("NewBotHandler: %v", err)
	}

	history := &dmHistorySpy{}
	stats := &pipelineStatsSpy{}
	testBot := &Bot{
		ctx:     context.Background(),
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		cfg:     cfg,
		history: history,
		stats:   stats,
		admin:   adminconfig.NewService(store, nil, nil),
		api:     botAPI,
	}
	testBot.registerRoutes(bh)

	chain := &pipelineTestBot{bot: testBot, bh: bh, api: api, history: history, stats: stats}
	if spy, ok := store.(*dmCatalogSpy); ok {
		chain.catalog = spy
	}

	return chain
}

func (p *pipelineTestBot) deliver(t *testing.T, update tg.Update) {
	t.Helper()

	if err := p.bh.BaseGroup().HandleUpdate(context.Background(), p.bot.api, update); err != nil {
		t.Fatalf("HandleUpdate: %v", err)
	}
}

func pipelinePrivateUpdate(fromID int64, text string) tg.Update {
	message := &tg.Message{
		MessageID: 11,
		Chat:      tg.Chat{ID: 100500, Type: tg.ChatTypePrivate},
		Text:      text,
	}
	if fromID != 0 {
		message.From = &tg.User{ID: fromID}
	}

	return tg.Update{UpdateID: 42, Message: message}
}

func pipelineGroupUpdate(fromID int64, text string) tg.Update {
	return tg.Update{UpdateID: 43, Message: &tg.Message{
		MessageID: 12,
		Chat:      tg.Chat{ID: -100999, Type: tg.ChatTypeSupergroup, Title: "Test Group"},
		Text:      text,
		From:      &tg.User{ID: fromID},
	}}
}

// TestUnknownCommandDroppedBeforeConversationPipeline proves a typo'd command
// and a removed legacy admin command never reach history or stats.
func TestUnknownCommandDroppedBeforeConversationPipeline(t *testing.T) {
	// dmCatalogSpy answers allow_all, so the access gate passes and the
	// unknown-command guard is what must drop each command.
	for _, text := range []string{"/config_show", "/admin_help", "/definitely_not_a_command"} {
		p := newPipelineTestBot(t, config.BotConfig{AdminIDs: []int64{7}}, &dmCatalogSpy{})

		p.deliver(t, pipelinePrivateUpdate(7, text))

		if got := p.history.appended.Load(); got != 0 {
			t.Fatalf("%s: unknown command must not reach history, got %d appends", text, got)
		}
		if got := p.stats.privateRequests.Load(); got != 0 {
			t.Fatalf("%s: unknown command must not be counted, got %d", text, got)
		}
	}
}

// TestUnknownGroupCommandDroppedBeforeLLM proves group commands never reach
// the conversational catch-all while plain group text does.
func TestUnknownGroupCommandDroppedBeforeLLM(t *testing.T) {
	p := newPipelineTestBot(t, config.BotConfig{AdminIDs: []int64{7}}, &dmCatalogSpy{})

	p.deliver(t, pipelineGroupUpdate(7, "/config_show"))
	if got := p.history.appended.Load(); got != 0 {
		t.Fatalf("group command must not reach history, got %d", got)
	}

	p.deliver(t, pipelineGroupUpdate(7, "plain group text"))
	if got := p.history.appended.Load(); got != 1 {
		t.Fatalf("plain group text must reach history, got %d", got)
	}
}

// TestStatsAndResetIsolatedFromHistoryAndRequestStats proves the
// administrative commands run before the ephemeral-state middlewares: their
// command messages no longer enter history or ordinary request statistics,
// while /reset still resets history and keeps its own metric.
func TestStatsAndResetIsolatedFromHistoryAndRequestStats(t *testing.T) {
	p := newPipelineTestBot(t, config.BotConfig{AdminIDs: []int64{7}}, &dmCatalogSpy{})

	p.deliver(t, pipelinePrivateUpdate(7, "/stats"))
	if got := p.history.appended.Load(); got != 0 {
		t.Fatalf("/stats must not enter history, got %d", got)
	}
	if got := p.stats.privateRequests.Load(); got != 0 {
		t.Fatalf("/stats must not count as a private request, got %d", got)
	}

	p.deliver(t, pipelinePrivateUpdate(7, "/reset"))
	if got := p.history.appended.Load(); got != 0 {
		t.Fatalf("/reset must not enter history, got %d", got)
	}
	if got := p.stats.privateRequests.Load(); got != 0 {
		t.Fatalf("/reset must not count as a private request, got %d", got)
	}
	if got := p.stats.chatResets.Load(); got != 1 {
		t.Fatalf("/reset must keep its own reset metric, got %d", got)
	}
	if got := p.api.lastText("sendMessage"); got != "Okay, let's start fresh." {
		t.Fatalf("/reset must still answer over the API, got %q", got)
	}
	if got := p.api.callCount("sendChatAction"); got == 0 {
		t.Fatalf("/reset must still send its typing indicator")
	}
}

// TestUserCommandsStillReachHandlers proves the remaining user commands pass
// the unknown-command guard and reach their handlers with history intact.
func TestUserCommandsStillReachHandlers(t *testing.T) {
	p := newPipelineTestBot(t, config.BotConfig{AdminIDs: []int64{7}}, &dmCatalogSpy{})

	// /start and /help sit after the history middlewares like before; /s is
	// an alias of /summarize (bare invocation answers usage help, which is
	// enough to prove its pipeline position). All must reach history first.
	for _, text := range []string{"/start", "/help", "/s"} {
		p.history.appended.Store(0)

		p.deliver(t, pipelinePrivateUpdate(7, text))

		if got := p.history.appended.Load(); got != 1 {
			t.Fatalf("%s must flow through history like before, got %d", text, got)
		}
	}
}

// TestBootstrapAdminCommandPassesAccessGate proves the narrow empty-admin-list
// exception: with BOT__ADMIN_IDS empty and the DM restriction off, a private
// /admin passes the access gate (the admin UI answers it with the setup
// diagnostic), while every other message from that chat stays dropped.
func TestBootstrapAdminCommandPassesAccessGate(t *testing.T) {
	p := newPipelineTestBot(t, config.BotConfig{}, &adminStoreStub{})

	// /admin passes the gate; until the admin UI adapter is wired it then
	// hits the unknown-command guard (dropped, no history) — observable here
	// as no history pollution and no crash.
	p.deliver(t, pipelinePrivateUpdate(42, "/admin"))
	if got := p.history.appended.Load(); got != 0 {
		t.Fatalf("bootstrap /admin must not pollute history, got %d", got)
	}

	// Other messages from the same unconfigured chat stay denied.
	p.deliver(t, pipelinePrivateUpdate(42, "hello"))
	if got := p.history.appended.Load(); got != 0 {
		t.Fatalf("ordinary message from unconfigured chat must stay dropped, got %d appends", got)
	}
}

// TestBootstrapExceptionRequiresDMRestrictionOff proves the exception cannot
// bypass BOT__DM_ADMINS_ONLY: with the flag on, every private message is
// dropped by the earlier gate before the access gate can admit it.
func TestBootstrapExceptionRequiresDMRestrictionOff(t *testing.T) {
	p := newPipelineTestBot(t, config.BotConfig{DMAdminsOnly: true}, &dmCatalogSpy{})

	p.deliver(t, pipelinePrivateUpdate(42, "/admin"))
	if got := p.catalog.upserts.Load(); got != 0 {
		t.Fatalf("DM restriction must drop /admin before any side effect, got %d catalog upserts", got)
	}
}

// TestIsBootstrapAdminCommand pins the exception predicate exactly.
func TestIsBootstrapAdminCommand(t *testing.T) {
	b := &Bot{cfg: config.BotConfig{}}

	updates := []struct {
		name string
		cfg  config.BotConfig
		text string
		chat string
		want bool
	}{
		{name: "empty admins private admin command", cfg: config.BotConfig{}, text: "/admin", chat: tg.ChatTypePrivate, want: true},
		{name: "configured admins never bootstrap", cfg: config.BotConfig{AdminIDs: []int64{7}}, text: "/admin", chat: tg.ChatTypePrivate, want: false},
		{name: "dm restriction on blocks exception", cfg: config.BotConfig{DMAdminsOnly: true}, text: "/admin", chat: tg.ChatTypePrivate, want: false},
		{name: "groups never bootstrap", cfg: config.BotConfig{}, text: "/admin", chat: tg.ChatTypeSupergroup, want: false},
		{name: "other commands never bootstrap", cfg: config.BotConfig{}, text: "/start", chat: tg.ChatTypePrivate, want: false},
		{name: "plain text never bootstrap", cfg: config.BotConfig{}, text: "hello", chat: tg.ChatTypePrivate, want: false},
	}

	for _, testCase := range updates {
		b.cfg = testCase.cfg

		message := &tg.Message{Chat: tg.Chat{Type: testCase.chat}, Text: testCase.text}
		if got := b.isBootstrapAdminCommand(message); got != testCase.want {
			t.Fatalf("%s: got %t want %t", testCase.name, got, testCase.want)
		}
	}
}

// TestNotAUserCommand pins the unknown-command predicate.
func TestNotAUserCommand(t *testing.T) {
	b := &Bot{}
	predicate := b.notAUserCommand()

	known := []string{"/start", "/help", "/summarize", "/s", "/stats", "/reset", "/start@somebot"}
	for _, text := range known {
		update := tg.Update{Message: &tg.Message{Text: text}}
		if predicate(context.Background(), update) {
			t.Fatalf("%s is a user command, must not match the unknown-command guard", text)
		}
	}

	unknown := []string{"/config_show", "/admin", "/cancel", "/definitely"}
	for _, text := range unknown {
		update := tg.Update{Message: &tg.Message{Text: text}}
		if !predicate(context.Background(), update) {
			t.Fatalf("%s is unknown, must match the guard", text)
		}
	}

	// Non-commands never match (they belong to the conversation pipeline).
	plain := tg.Update{Message: &tg.Message{Text: "hello there"}}
	if predicate(context.Background(), plain) {
		t.Fatalf("plain text must not match the unknown-command guard")
	}
}
