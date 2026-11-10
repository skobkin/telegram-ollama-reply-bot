package bot

import (
	"context"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"

	"telegram-ollama-reply-bot/internal/adminconfig"
	"telegram-ollama-reply-bot/internal/config"
	"telegram-ollama-reply-bot/internal/state"

	tg "github.com/mymmrac/telego"
	th "github.com/mymmrac/telego/telegohandler"
)

// dmHistorySpy counts history appends without storing anything.
type dmHistorySpy struct {
	appended atomic.Int32
}

func (s *dmHistorySpy) AppendMessage(state.ConversationScope, state.Message) {
	s.appended.Add(1)
}

func (s *dmHistorySpy) Snapshot(state.ConversationScope) state.ConversationSnapshot {
	return state.ConversationSnapshot{Messages: make([]state.Message, 0)}
}

func (s *dmHistorySpy) Search(state.ConversationScope, state.HistorySearchQuery) []state.HistorySearchMatch {
	return nil
}

func (s *dmHistorySpy) SetEarlierSummary(state.ConversationScope, string, int) {}
func (s *dmHistorySpy) Reset(state.ConversationScope)                          {}
func (s *dmHistorySpy) ResetChat(int64)                                        {}

// dmStatsSpy counts the request counters the middleware chain touches.
type dmStatsSpy struct {
	privateRequests atomic.Int32
	groupRequests   atomic.Int32
}

func (s *dmStatsSpy) Snapshot() state.StatsSnapshot { return state.StatsSnapshot{} }
func (s *dmStatsSpy) String() string                { return "" }
func (s *dmStatsSpy) InlineQuery()                  {}
func (s *dmStatsSpy) GroupRequest()                 { s.groupRequests.Add(1) }
func (s *dmStatsSpy) PrivateRequest()               { s.privateRequests.Add(1) }
func (s *dmStatsSpy) Mention()                      {}
func (s *dmStatsSpy) SummarizeRequest()             {}
func (s *dmStatsSpy) ChatHistoryReset()             {}
func (s *dmStatsSpy) LlmTimeout()                   {}
func (s *dmStatsSpy) AddUsage(int, int, int, float64) {
}

// dmCatalogSpy counts catalog upserts on top of the shared admin store stub.
type dmCatalogSpy struct {
	adminStoreStub
	upserts atomic.Int32
}

func (s *dmCatalogSpy) UpsertChatCatalog(context.Context, adminconfig.ChatCatalogEntry) error {
	s.upserts.Add(1)

	return nil
}

// GetAccessPolicy isolates these tests from the access layer: the DM gate is
// the concern here, so the chain must pass the access gate unconditionally.
func (s *dmCatalogSpy) GetAccessPolicy(context.Context) (adminconfig.AccessPolicy, error) {
	return adminconfig.AccessPolicyAllowAll, nil
}

// runDMGateChain pushes one update through the REAL registerRoutes pipeline
// (request logger → DM gate → catalog → access gate → admin routes → command
// guards → history → stats → handlers) and reports which side effects fired.
// "Reached" means the update made it past every gate into the ephemeral-state
// middlewares — the DM-gate ordering concern — observed via the history spy
// (the conversational catch-all terminates the chain, so a trailing marker
// handler would never fire). The base group is driven synchronously via
// HandleUpdate: BotHandler.Start dispatches updates in goroutines and returns
// on channel close, which would race the completion checks.
func runDMGateChain(t *testing.T, cfg config.BotConfig, update tg.Update) (reached bool, history *dmHistorySpy, stats *dmStatsSpy, catalog *dmCatalogSpy) {
	t.Helper()

	bh, err := th.NewBotHandler(nil, make(chan tg.Update))
	if err != nil {
		t.Fatalf("NewBotHandler: %v", err)
	}

	history = &dmHistorySpy{}
	stats = &dmStatsSpy{}
	catalog = &dmCatalogSpy{}

	testBot := &Bot{
		ctx:     context.Background(),
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		cfg:     cfg,
		history: history,
		stats:   stats,
		admin:   adminconfig.NewService(catalog, nil, nil),
	}
	testBot.registerRoutes(bh)

	if err := bh.BaseGroup().HandleUpdate(context.Background(), nil, update); err != nil {
		t.Fatalf("HandleUpdate: %v", err)
	}

	reached = history.appended.Load() > 0

	return reached, history, stats, catalog
}

func dmPrivateUpdate(fromID int64, text string) tg.Update {
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

// TestDMAdminsOnlyGateFlagFalseKeepsPrivateFlow is the DM-gate contract: with
// the flag off (the default), a private message from an access-allowed chat
// flows through the whole chain — cataloged, stored, and counted. (The access
// policy itself is allow_all in this fixture; it is covered separately.)
func TestDMAdminsOnlyGateFlagFalseKeepsPrivateFlow(t *testing.T) {
	reached, history, stats, catalog := runDMGateChain(t,
		config.BotConfig{AdminIDs: []int64{7}},
		dmPrivateUpdate(2, "hello"))

	if !reached {
		t.Fatal("flag off must not stop the chain for private messages")
	}
	if got := history.appended.Load(); got != 1 {
		t.Fatalf("expected history append, got %d", got)
	}
	if got := stats.privateRequests.Load(); got != 1 {
		t.Fatalf("expected private request counter, got %d", got)
	}
	if got := catalog.upserts.Load(); got != 1 {
		t.Fatalf("expected catalog upsert, got %d", got)
	}
}

// TestDMAdminsOnlyGateAllowsAdminDM proves admins (BOT__ADMIN_IDS) keep full
// private-chat access while the restriction is enabled.
func TestDMAdminsOnlyGateAllowsAdminDM(t *testing.T) {
	reached, history, stats, catalog := runDMGateChain(t,
		config.BotConfig{AdminIDs: []int64{7}, DMAdminsOnly: true},
		dmPrivateUpdate(7, "admin hello"))

	if !reached {
		t.Fatal("admin DM must reach the handlers")
	}
	if got := history.appended.Load(); got != 1 {
		t.Fatalf("expected history append for admin DM, got %d", got)
	}
	if got := stats.privateRequests.Load(); got != 1 {
		t.Fatalf("expected private request counter for admin DM, got %d", got)
	}
	if got := catalog.upserts.Load(); got != 1 {
		t.Fatalf("expected catalog upsert for admin DM, got %d", got)
	}
}

// TestDMAdminsOnlyGateDropsNonAdminBeforeSideEffects proves the gate stops the
// chain before the catalog middleware (the first side effect) for ordinary
// non-admin private text: nothing is cataloged, stored, or counted, and no
// handler (conversation, LLM, commands) ever runs.
func TestDMAdminsOnlyGateDropsNonAdminBeforeSideEffects(t *testing.T) {
	reached, history, stats, catalog := runDMGateChain(t,
		config.BotConfig{AdminIDs: []int64{7}, DMAdminsOnly: true},
		dmPrivateUpdate(2, "hello"))

	if reached {
		t.Fatal("non-admin DM must be dropped before handlers")
	}
	if got := history.appended.Load(); got != 0 {
		t.Fatalf("denied DM must not reach history, got %d appends", got)
	}
	if got := stats.privateRequests.Load(); got != 0 {
		t.Fatalf("denied DM must not be counted, got %d", got)
	}
	if got := catalog.upserts.Load(); got != 0 {
		t.Fatalf("denied DM must not be cataloged (gate must precede catalog), got %d", got)
	}
}

// TestDMAdminsOnlyGateDropsNonAdminPhotoAndCommands covers the payload kinds
// that matter: photo posts and the dedicated commands — including /summarize,
// which would otherwise trigger an extractor fetch plus an LLM call for any
// private user. The marker handler stands in for all of them: not reached
// means zero command side effects.
func TestDMAdminsOnlyGateDropsNonAdminPhotoAndCommands(t *testing.T) {
	photoUpdate := tg.Update{UpdateID: 43, Message: &tg.Message{
		MessageID: 12,
		Chat:      tg.Chat{ID: 100500, Type: tg.ChatTypePrivate},
		From:      &tg.User{ID: 2},
		Photo:     []tg.PhotoSize{{FileID: "f", Width: 1, Height: 1}},
	}}

	messages := map[string]tg.Update{
		"photo":     photoUpdate,
		"summarize": dmPrivateUpdate(2, "/summarize https://example.com/article"),
		"stats":     dmPrivateUpdate(2, "/stats"),
		"reset":     dmPrivateUpdate(2, "/reset"),
		"start":     dmPrivateUpdate(2, "/start"),
		"help":      dmPrivateUpdate(2, "/help"),
	}

	for name, update := range messages {
		t.Run(name, func(t *testing.T) {
			reached, history, stats, catalog := runDMGateChain(t,
				config.BotConfig{AdminIDs: []int64{7}, DMAdminsOnly: true},
				update)

			if reached {
				t.Fatal("non-admin DM must be dropped before command handlers")
			}
			if got := history.appended.Load(); got != 0 {
				t.Fatalf("denied DM must not reach history, got %d appends", got)
			}
			if got := stats.privateRequests.Load(); got != 0 {
				t.Fatalf("denied DM must not be counted, got %d", got)
			}
			if got := catalog.upserts.Load(); got != 0 {
				t.Fatalf("denied DM must not be cataloged, got %d", got)
			}
		})
	}
}

// TestDMAdminsOnlyGateDropsNilSender proves the deny path is safe for private
// messages without a From user (the gate must not dereference it, and such a
// message can never prove adminship).
func TestDMAdminsOnlyGateDropsNilSender(t *testing.T) {
	reached, _, _, catalog := runDMGateChain(t,
		config.BotConfig{AdminIDs: []int64{7}, DMAdminsOnly: true},
		dmPrivateUpdate(0, "anonymous"))

	if reached {
		t.Fatal("private message without From must be dropped")
	}
	if got := catalog.upserts.Load(); got != 0 {
		t.Fatalf("denied DM must not be cataloged, got %d", got)
	}
}

// TestDMAdminsOnlyGateEmptyAdminsDropsEveryone pins the documented sharp edge:
// flag on + empty BOT__ADMIN_IDS means nobody is an admin, so ALL private
// messages (including from the would-be operator) are dropped.
func TestDMAdminsOnlyGateEmptyAdminsDropsEveryone(t *testing.T) {
	reached, history, _, _ := runDMGateChain(t,
		config.BotConfig{DMAdminsOnly: true},
		dmPrivateUpdate(7, "let me in"))

	if reached {
		t.Fatal("with empty admin list every private message must be dropped")
	}
	if got := history.appended.Load(); got != 0 {
		t.Fatalf("denied DM must not reach history, got %d appends", got)
	}
}

// TestDMAdminsOnlyGateLeavesGroupsUnchanged proves the restriction is
// DM-specific: with the flag on, group messages from non-admins keep flowing
// through the whole chain (whitelist and interactivity still apply downstream).
func TestDMAdminsOnlyGateLeavesGroupsUnchanged(t *testing.T) {
	groupUpdate := tg.Update{UpdateID: 44, Message: &tg.Message{
		MessageID: 13,
		Chat:      tg.Chat{ID: -100200, Type: tg.ChatTypeSupergroup},
		From:      &tg.User{ID: 2},
		Text:      "group hello",
	}}

	reached, history, _, catalog := runDMGateChain(t,
		config.BotConfig{AdminIDs: []int64{7}, DMAdminsOnly: true},
		groupUpdate)

	if !reached {
		t.Fatal("group messages must not be affected by the DM restriction")
	}
	if got := history.appended.Load(); got != 1 {
		t.Fatalf("expected history append for group message, got %d", got)
	}
	if got := catalog.upserts.Load(); got != 1 {
		t.Fatalf("expected catalog upsert for group message, got %d", got)
	}
}
