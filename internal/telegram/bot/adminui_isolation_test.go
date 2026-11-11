package bot

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	tg "github.com/mymmrac/telego"
	th "github.com/mymmrac/telego/telegohandler"

	"telegram-ollama-reply-bot/internal/adminconfig"
	core "telegram-ollama-reply-bot/internal/adminui"
	"telegram-ollama-reply-bot/internal/config"
	adminui "telegram-ollama-reply-bot/internal/telegram/adminui"
)

const (
	fusedBotID  = int64(999)
	fusedAdmin  = int64(7)
	plainUserID = int64(42)
)

// adminChainBot fuses the real Bot pipeline with the real admin UI chain
// (controller + manager + adapter): the fused harness of the test plan. It
// drives updates synchronously through BaseGroup().HandleUpdate and exposes
// the spies so isolation can be asserted per update.
type adminChainBot struct {
	bot     *Bot
	bh      *th.BotHandler
	api     *fakeAPIServer
	history *dmHistorySpy
	stats   *pipelineStatsSpy
	manager *core.Manager
}

func newAdminChainBot(t *testing.T, cfg config.BotConfig, store adminconfig.Store) *adminChainBot {
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
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	manager := core.NewManager(nil)
	controller := core.NewController(manager, adminconfig.NewService(store, nil, nil), cfg.AdminIDs, nil)

	testBot := &Bot{
		ctx:     context.Background(),
		logger:  logger,
		cfg:     cfg,
		history: history,
		stats:   stats,
		admin:   adminconfig.NewService(store, nil, nil),
		api:     botAPI,
		me:      botInfo{ID: fusedBotID, Username: "test_bot"},
	}
	testBot.SetAdminUI(adminui.NewAdapter(controller, manager, cfg.AdminIDs, logger))
	testBot.registerRoutes(bh)

	return &adminChainBot{bot: testBot, bh: bh, api: api, history: history, stats: stats, manager: manager}
}

func (c *adminChainBot) deliver(t *testing.T, update tg.Update) {
	t.Helper()

	if err := c.bh.BaseGroup().HandleUpdate(context.Background(), c.bot.api, update); err != nil {
		t.Fatalf("HandleUpdate: %v", err)
	}
}

// fusedUpdate builders.

func fusedPrivate(fromID int64, text string) tg.Update {
	return tg.Update{UpdateID: 50, Message: &tg.Message{
		MessageID: 60,
		Chat:      tg.Chat{ID: 100500, Type: tg.ChatTypePrivate},
		From:      &tg.User{ID: fromID},
		Text:      text,
	}}
}

func fusedCallback(fromID int64, msgID int, data string) tg.Update {
	return tg.Update{UpdateID: 51, CallbackQuery: &tg.CallbackQuery{
		ID:   "cb-1",
		From: tg.User{ID: fromID},
		Message: &tg.Message{
			MessageID: msgID,
			// A bot DM's chat ID equals the user's own ID: the callback
			// origin check (callbackOrigin) requires exactly that, so the
			// callback is processed for real instead of bounced as gone.
			Chat: tg.Chat{ID: fromID, Type: tg.ChatTypePrivate},
		},
		Data: data,
	}}
}

func fusedReply(text string, replyTo tg.Message) tg.Update {
	return tg.Update{UpdateID: 52, Message: &tg.Message{
		MessageID:      61,
		Chat:           tg.Chat{ID: 100500, Type: tg.ChatTypePrivate},
		From:           &tg.User{ID: fusedAdmin},
		Text:           text,
		ReplyToMessage: &replyTo,
	}}
}

func fusedBotMessage(msgID int, text string) tg.Message {
	return tg.Message{
		MessageID: msgID,
		Chat:      tg.Chat{ID: 100500, Type: tg.ChatTypePrivate},
		From:      &tg.User{ID: fusedBotID, IsBot: true},
		Text:      text,
	}
}

func fusedChatShared(requestID int, chatID int64) tg.Update {
	return tg.Update{UpdateID: 53, Message: &tg.Message{
		MessageID: 62,
		Chat:      tg.Chat{ID: 100500, Type: tg.ChatTypePrivate},
		From:      &tg.User{ID: fusedAdmin},
		ChatShared: &tg.ChatShared{
			RequestID: requestID,
			ChatID:    chatID,
			Title:     "Shared Group",
		},
	}}
}

// tap fires a callback on the live fused session.
func (c *adminChainBot) tap(t *testing.T, verb string, args ...string) {
	t.Helper()

	session := c.manager.Session(fusedAdmin)
	if session == nil {
		t.Fatalf("no fused session to tap")
	}
	_, _, msgID, _ := session.DetachedSnapshot()
	c.deliver(t, fusedCallback(fusedAdmin, msgID, core.EncodeAction(verb, session.Seq, args...)))
}

// TestUnmarkedReplyToLivePendingNeverReachesPipeline pins the classification
// order in the real pipeline: pending correlation beats marker
// classification, so a reply matching the live pending's prompt message is
// consumed as ADMIN INPUT — applied to the store and isolated from history
// and stats — even when the marker text is absent or altered. Without this
// ordering an altered marker turned the reply into ordinary conversation
// text and leaked it to the LLM.
func TestUnmarkedReplyToLivePendingNeverReachesPipeline(t *testing.T) {
	armLanguageInput := func(t *testing.T, c *adminChainBot) int {
		t.Helper()

		c.deliver(t, fusedPrivate(fusedAdmin, "/admin"))
		c.tap(t, core.VerbGlobalSet)
		c.tap(t, core.VerbField, "lg")
		c.tap(t, core.VerbString, "lg")

		session := c.manager.Session(fusedAdmin)
		if session.Pending == nil || session.Pending.PromptMsgID == 0 {
			t.Fatalf("pending input must be armed and correlated")
		}

		return session.Pending.PromptMsgID
	}

	t.Run("altered marker", func(t *testing.T) {
		spy := &dmCatalogSpy{}
		c := newAdminChainBot(t, config.BotConfig{AdminIDs: []int64{fusedAdmin}}, spy)
		promptID := armLanguageInput(t, c)

		c.deliver(t, fusedReply("Русский", fusedBotMessage(promptID, "someone edited this text away")))

		if got := spy.writes.Load(); got != 1 {
			t.Fatalf("the unmarked reply must be applied as admin input, got %d writes", got)
		}
		c.assertIsolated(t, "altered-marker reply")
	})

	t.Run("stripped marker text", func(t *testing.T) {
		spy := &dmCatalogSpy{}
		c := newAdminChainBot(t, config.BotConfig{AdminIDs: []int64{fusedAdmin}}, spy)
		promptID := armLanguageInput(t, c)

		// Telegram strips replied-to content entirely for some message kinds:
		// no text to classify by, but the pending still matches.
		c.deliver(t, fusedReply("Русский", fusedBotMessage(promptID, "")))

		if got := spy.writes.Load(); got != 1 {
			t.Fatalf("the reply matched by the pending must be applied, got %d writes", got)
		}
		c.assertIsolated(t, "stripped-marker reply")
	})

	t.Run("altered text without pending falls through", func(t *testing.T) {
		spy := &dmCatalogSpy{}
		c := newAdminChainBot(t, config.BotConfig{AdminIDs: []int64{fusedAdmin}}, spy)

		ordinary := fusedBotMessage(73, "some ordinary bot answer, no marker left")
		c.deliver(t, fusedReply("thanks!", ordinary))

		if got := spy.writes.Load(); got != 0 {
			t.Fatalf("an ordinary reply must not be consumed as admin input, got %d writes", got)
		}
		if got := c.history.appended.Load(); got != 1 {
			t.Fatalf("ordinary replies must keep reaching the pipeline, got %d appends", got)
		}
		if got := c.stats.privateRequests.Load(); got != 1 {
			t.Fatalf("ordinary replies must keep being counted, got %d", got)
		}
	})
}

// TestAdminUIUpdatesNeverReachConversationPipeline proves every
// administrative update — admin commands, callbacks, correlated replies,
// fail-closed replies, chat selections — terminates inside the admin UI
// routes: no conversation history, no ordinary request statistics.
func TestAdminUIUpdatesNeverReachConversationPipeline(t *testing.T) {
	t.Run("admin command", func(t *testing.T) {
		c := newAdminChainBot(t, config.BotConfig{AdminIDs: []int64{fusedAdmin}}, &dmCatalogSpy{})
		c.deliver(t, fusedPrivate(fusedAdmin, "/admin"))

		c.assertIsolated(t, "/admin")
	})

	t.Run("cancel command", func(t *testing.T) {
		c := newAdminChainBot(t, config.BotConfig{AdminIDs: []int64{fusedAdmin}}, &dmCatalogSpy{})
		c.deliver(t, fusedPrivate(fusedAdmin, "/cancel"))

		c.assertIsolated(t, "/cancel")
	})

	t.Run("callback after open", func(t *testing.T) {
		c := newAdminChainBot(t, config.BotConfig{AdminIDs: []int64{fusedAdmin}}, &dmCatalogSpy{})
		c.deliver(t, fusedPrivate(fusedAdmin, "/admin"))

		session := c.manager.Session(fusedAdmin)
		if session == nil {
			t.Fatalf("admin session missing after /admin")
		}
		_, _, msgID, _ := session.DetachedSnapshot()
		c.deliver(t, fusedCallback(fusedAdmin, msgID, core.EncodeAction(core.VerbCatalog, session.Seq, "1")))

		if got := c.api.callCount("answerCallbackQuery"); got != 1 {
			t.Fatalf("callback answered exactly once, got %d", got)
		}
		c.assertIsolated(t, "callback")
	})

	t.Run("marker reply without session", func(t *testing.T) {
		c := newAdminChainBot(t, config.BotConfig{AdminIDs: []int64{fusedAdmin}}, &dmCatalogSpy{})
		prompt := fusedBotMessage(70, core.AdminInputMarker+" — Language")
		c.deliver(t, fusedReply("Русский", prompt))

		c.assertIsolated(t, "marker reply")
	})

	t.Run("unclassifiable reply fails closed", func(t *testing.T) {
		c := newAdminChainBot(t, config.BotConfig{AdminIDs: []int64{fusedAdmin}}, &dmCatalogSpy{})
		media := fusedBotMessage(71, "") // no text to classify by
		c.deliver(t, fusedReply("a caption", media))

		c.assertIsolated(t, "media reply")
	})

	t.Run("chat selection without session", func(t *testing.T) {
		c := newAdminChainBot(t, config.BotConfig{AdminIDs: []int64{fusedAdmin}}, &dmCatalogSpy{})
		c.deliver(t, fusedChatShared(777, -1001234567890))

		c.assertIsolated(t, "chat shared")
	})
}

func (c *adminChainBot) assertIsolated(t *testing.T, what string) {
	t.Helper()

	if got := c.history.appended.Load(); got != 0 {
		t.Fatalf("%s must not reach conversation history, got %d appends", what, got)
	}
	if got := c.stats.privateRequests.Load(); got != 0 {
		t.Fatalf("%s must not count as an ordinary request, got %d", what, got)
	}
}

// TestAdminDMOrdinaryTextStillReachesPipeline proves the isolation is not
// overreach: a plain admin DM message keeps flowing through history, stats,
// and the LLM pipeline as before.
func TestAdminDMOrdinaryTextStillReachesPipeline(t *testing.T) {
	c := newAdminChainBot(t, config.BotConfig{AdminIDs: []int64{fusedAdmin}}, &dmCatalogSpy{})

	c.deliver(t, fusedPrivate(fusedAdmin, "hello there"))

	if got := c.history.appended.Load(); got != 1 {
		t.Fatalf("ordinary admin DM text must reach history, got %d", got)
	}
	if got := c.stats.privateRequests.Load(); got != 1 {
		t.Fatalf("ordinary admin DM text must be counted, got %d", got)
	}
}

// TestReplyToOrdinaryBotMessageResumesChain proves a positively-distinguished
// ordinary reply falls through the admin input route back into the pipeline.
func TestReplyToOrdinaryBotMessageResumesChain(t *testing.T) {
	c := newAdminChainBot(t, config.BotConfig{AdminIDs: []int64{fusedAdmin}}, &dmCatalogSpy{})

	ordinary := fusedBotMessage(72, "here is my ordinary answer")
	c.deliver(t, fusedReply("thanks!", ordinary))

	if got := c.history.appended.Load(); got != 1 {
		t.Fatalf("ordinary reply must resume the chain, got %d appends", got)
	}
	if got := c.stats.privateRequests.Load(); got != 1 {
		t.Fatalf("ordinary reply must be counted, got %d", got)
	}
}

// TestUnknownCommandDroppedWithAdminUIWired re-proves the unknown-command
// guard with the admin UI routes registered in front of it.
func TestUnknownCommandDroppedWithAdminUIWired(t *testing.T) {
	c := newAdminChainBot(t, config.BotConfig{AdminIDs: []int64{fusedAdmin}}, &dmCatalogSpy{})

	c.deliver(t, fusedPrivate(fusedAdmin, "/config_show"))

	c.assertIsolated(t, "/config_show")
}

// TestBootstrapDiagnosticOnEmptyAdminIDs proves the narrow access-gate
// exception: with no admins configured and the default allowlist policy, a
// private /admin gets the setup diagnostic (including the sender's ID), while
// every other message from that chat is still silently dropped.
func TestBootstrapDiagnosticOnEmptyAdminIDs(t *testing.T) {
	c := newAdminChainBot(t, config.BotConfig{}, &adminStoreStub{})

	c.deliver(t, fusedPrivate(plainUserID, "/admin"))

	if text := c.api.lastText("sendMessage"); !strings.Contains(text, "Admin controls are disabled") ||
		!strings.Contains(text, "42") {
		t.Fatalf("bootstrap diagnostic expected, got %q", text)
	}

	requests := c.api.callCount("sendMessage")
	c.deliver(t, fusedPrivate(plainUserID, "hello"))
	if got := c.api.callCount("sendMessage"); got != requests {
		t.Fatalf("ordinary messages must stay dropped, sends went %d → %d", requests, got)
	}
	if got := c.history.appended.Load(); got != 0 {
		t.Fatalf("ordinary messages must not reach history, got %d", got)
	}
}

// TestBootstrapDiagnosticDroppedWithDMAdminsOnly proves the DM gate still
// wins when BOT__DM_ADMINS_ONLY is on with no admins: the bootstrap exception
// cannot bypass it.
func TestBootstrapDiagnosticDroppedWithDMAdminsOnly(t *testing.T) {
	c := newAdminChainBot(t, config.BotConfig{DMAdminsOnly: true}, &adminStoreStub{})

	c.deliver(t, fusedPrivate(plainUserID, "/admin"))

	if got := c.api.callCount("sendMessage"); got != 0 {
		t.Fatalf("bootstrap diagnostic must stay dropped with DM_ADMINS_ONLY, got %d sends", got)
	}
}

// TestNonAdminAdminCommandDeniedUnderAllowAll proves the handler-level
// denial for non-admins when the access policy lets the message through.
func TestNonAdminAdminCommandDeniedUnderAllowAll(t *testing.T) {
	c := newAdminChainBot(t, config.BotConfig{AdminIDs: []int64{fusedAdmin}}, &dmCatalogSpy{})

	c.deliver(t, fusedPrivate(plainUserID, "/cancel"))

	if text := c.api.lastText("sendMessage"); !strings.Contains(text, "only to administrators") {
		t.Fatalf("non-admin denial expected, got %q", text)
	}
	c.assertIsolated(t, "non-admin /cancel")
}
