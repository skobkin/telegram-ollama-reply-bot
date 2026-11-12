package bot

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"telegram-ollama-reply-bot/internal/adminconfig"
	"telegram-ollama-reply-bot/internal/config"
	"telegram-ollama-reply-bot/internal/state"
	"telegram-ollama-reply-bot/internal/state/memory"
	"telegram-ollama-reply-bot/internal/support/markdown"

	tg "github.com/mymmrac/telego"
	th "github.com/mymmrac/telego/telegohandler"
)

// reactionTestBot builds the real route pipeline over a caller-provided
// history store, for tests that assert on stored history content.
type reactionTestBot struct {
	bot   *Bot
	bh    *th.BotHandler
	api   *fakeAPIServer
	store state.ConversationStore
}

func newReactionTestBot(t *testing.T, store adminconfig.Store, history state.ConversationStore) *reactionTestBot {
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

	testBot := &Bot{
		ctx:       context.Background(),
		logger:    reactionTestLogger(),
		cfg:       config.BotConfig{AdminIDs: []int64{7}},
		history:   history,
		stats:     &pipelineStatsSpy{},
		admin:     adminconfig.NewService(store, nil, nil),
		api:       botAPI,
		sanitizer: markdown.NewTgMarkdownV2Sanitizer(),
	}
	testBot.registerRoutes(bh)

	return &reactionTestBot{bot: testBot, bh: bh, api: api, store: history}
}

func (p *reactionTestBot) deliver(t *testing.T, update tg.Update) {
	t.Helper()

	if err := p.bh.BaseGroup().HandleUpdate(context.Background(), p.bot.api, update); err != nil {
		t.Fatalf("HandleUpdate: %v", err)
	}
}

func reactionTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newMemoryConversationStore() state.ConversationStore {
	return memory.New(memory.Config{}, reactionTestLogger()).Conversations()
}

func reactionUpdate(chatID int64, messageID int, user *tg.User, newReaction []tg.ReactionType) tg.Update {
	return reactionUpdateWithID(44, chatID, messageID, user, newReaction)
}

// reactionUpdateWithID lets tests set the envelope UpdateID, which the
// handler carries into the store as the update-order sequence.
func reactionUpdateWithID(updateID int, chatID int64, messageID int, user *tg.User, newReaction []tg.ReactionType) tg.Update {
	return tg.Update{UpdateID: updateID, MessageReaction: &tg.MessageReactionUpdated{
		Chat:        tg.Chat{ID: chatID, Type: tg.ChatTypePrivate},
		MessageID:   messageID,
		User:        user,
		Date:        1700000000,
		NewReaction: newReaction,
	}}
}

// emojiReaction builds a Telegram-shaped emoji reaction: the type
// discriminant must be set or telego's update re-marshaling rejects it.
func emojiReaction(emoji string) []tg.ReactionType {
	return []tg.ReactionType{&tg.ReactionTypeEmoji{Type: tg.ReactionEmoji, Emoji: emoji}}
}

// TestReactionRecordedInHistory proves a whitelisted reaction on a bot reply
// is stored as metadata on the history entry with reactor identity.
func TestReactionRecordedInHistory(t *testing.T) {
	p := newReactionTestBot(t, &dmCatalogSpy{}, newMemoryConversationStore())

	scope := state.ConversationScope{ChatID: 100500}
	p.store.AppendMessage(scope, state.Message{IsMe: true, MessageID: 777, Text: "here is the answer"})

	p.deliver(t, reactionUpdate(100500, 777, &tg.User{ID: 42, FirstName: "Alice", Username: "alice"}, emojiReaction("👍")))

	snapshot := p.store.Snapshot(scope)
	if len(snapshot.Messages) != 1 || len(snapshot.Messages[0].Reactions) != 1 {
		t.Fatalf("expected one reaction on the bot message, got %+v", snapshot.Messages)
	}
	reaction := snapshot.Messages[0].Reactions[0]
	if reaction.UserID != 42 || reaction.Emoji != "👍" || reaction.Name != "Alice" || reaction.Username != "alice" {
		t.Fatalf("unexpected reaction: %+v", reaction)
	}
	if reaction.UpdatedAt.IsZero() {
		t.Fatalf("reaction timestamp not set: %+v", reaction)
	}
}

// TestReactionReplaceAndRemovalPerUser proves the per-user replace semantics:
// a changed reaction replaces the old one, and an empty NewReaction clears
// only that user's reactions.
func TestReactionReplaceAndRemovalPerUser(t *testing.T) {
	p := newReactionTestBot(t, &dmCatalogSpy{}, newMemoryConversationStore())

	scope := state.ConversationScope{ChatID: 100500}
	p.store.AppendMessage(scope, state.Message{IsMe: true, MessageID: 777, Text: "answer"})

	// Each delivery carries a strictly increasing Telegram update ID, as in
	// reality; the store rejects anything not newer per user and message.
	p.deliver(t, reactionUpdateWithID(101, 100500, 777, &tg.User{ID: 42, FirstName: "Alice"}, emojiReaction("👍")))
	p.deliver(t, reactionUpdateWithID(102, 100500, 777, &tg.User{ID: 42, FirstName: "Alice"}, emojiReaction("👎")))
	p.deliver(t, reactionUpdateWithID(103, 100500, 777, &tg.User{ID: 43, FirstName: "Bob"}, emojiReaction("🔥")))

	reactions := p.store.Snapshot(scope).Messages[0].Reactions
	if len(reactions) != 2 {
		t.Fatalf("expected Alice's replace plus Bob's reaction, got %+v", reactions)
	}
	if reactions[0].UserID != 42 || reactions[0].Emoji != "👎" {
		t.Fatalf("Alice's reaction must be replaced with the new emoji, got %+v", reactions[0])
	}
	if reactions[1].UserID != 43 || reactions[1].Emoji != "🔥" {
		t.Fatalf("unexpected Bob reaction: %+v", reactions[1])
	}

	// Alice removes her reaction entirely (the update still carries the old
	// reaction list, as Telegram does).
	removal := reactionUpdateWithID(104, 100500, 777, &tg.User{ID: 42, FirstName: "Alice"}, nil)
	removal.MessageReaction.OldReaction = emojiReaction("👎")
	p.deliver(t, removal)

	reactions = p.store.Snapshot(scope).Messages[0].Reactions
	if len(reactions) != 1 || reactions[0].UserID != 43 {
		t.Fatalf("removal must clear only Alice's reactions, got %+v", reactions)
	}
}

// TestReactionIgnoredCases proves reactions that carry no usable feedback
// never reach the history store: anonymous actors, untracked emoji, and
// premium custom-emoji or paid reaction types.
func TestReactionIgnoredCases(t *testing.T) {
	spy := &dmHistorySpy{}
	p := newReactionTestBot(t, &dmCatalogSpy{}, spy)

	cases := []struct {
		name    string
		user    *tg.User
		newList []tg.ReactionType
	}{
		{"anonymous", nil, emojiReaction("👍")},
		{"untracked emoji", &tg.User{ID: 42, FirstName: "Alice"}, emojiReaction("🍌")},
		{"custom emoji", &tg.User{ID: 42, FirstName: "Alice"}, []tg.ReactionType{&tg.ReactionTypeCustomEmoji{Type: tg.ReactionCustomEmoji, CustomEmojiID: "42"}}},
		{"paid", &tg.User{ID: 42, FirstName: "Alice"}, []tg.ReactionType{&tg.ReactionTypePaid{Type: tg.ReactionPaid}}},
	}

	for _, testCase := range cases {
		p.deliver(t, reactionUpdate(100500, 777, testCase.user, testCase.newList))

		if got := spy.reactionCallCount(); got != 0 {
			t.Fatalf("%s: reaction must be ignored, got %d store calls", testCase.name, got)
		}
	}
}

// TestReactionChangeAwayFromTrackedEmojiClears proves a user switching from a
// tracked emoji to an untracked one still records the removal.
func TestReactionChangeAwayFromTrackedEmojiClears(t *testing.T) {
	spy := &dmHistorySpy{reactionFound: true}
	p := newReactionTestBot(t, &dmCatalogSpy{}, spy)

	p.deliver(t, reactionUpdateWithID(201, 100500, 777, &tg.User{ID: 42, FirstName: "Alice"}, emojiReaction("👍")))
	if got := spy.reactionCallCount(); got != 1 {
		t.Fatalf("tracked reaction must reach the store, got %d calls", got)
	}

	// A real emoji switch carries the previous reaction in OldReaction.
	switchAway := reactionUpdateWithID(202, 100500, 777, &tg.User{ID: 42, FirstName: "Alice"}, emojiReaction("🍌"))
	switchAway.MessageReaction.OldReaction = emojiReaction("👍")
	p.deliver(t, switchAway)

	call := spy.lastReactionCall()
	if got := spy.reactionCallCount(); got != 2 || len(call.reactions) != 0 {
		t.Fatalf("switching to untracked emoji must clear reactions, got %d calls, reactions %+v", got, call.reactions)
	}
}

// TestReactionUnmappedTargetHarmless proves a reaction on a message that is
// not in bot history (never mapped, restart, trim, reset) is a no-op.
func TestReactionUnmappedTargetHarmless(t *testing.T) {
	p := newReactionTestBot(t, &dmCatalogSpy{}, newMemoryConversationStore())

	scope := state.ConversationScope{ChatID: 100500}
	p.store.AppendMessage(scope, state.Message{IsMe: true, MessageID: 777, Text: "answer"})

	// Unknown message ID and a reaction on a user message both miss.
	p.deliver(t, reactionUpdate(100500, 999, &tg.User{ID: 42, FirstName: "Alice"}, emojiReaction("👍")))
	p.deliver(t, reactionUpdate(100500, 12, &tg.User{ID: 42, FirstName: "Alice"}, emojiReaction("👍")))

	snapshot := p.store.Snapshot(scope)
	if len(snapshot.Messages[0].Reactions) != 0 {
		t.Fatalf("unmapped reactions must not modify history, got %+v", snapshot.Messages[0].Reactions)
	}
}

// TestReactionOutOfOrderUpdatesCannotResurrect proves update-order safety:
// handlers run concurrently, so a delayed "add" update arriving after the
// user's later "remove" must not resurrect the removed reaction.
func TestReactionOutOfOrderUpdatesCannotResurrect(t *testing.T) {
	p := newReactionTestBot(t, &dmCatalogSpy{}, newMemoryConversationStore())

	scope := state.ConversationScope{ChatID: 100500}
	p.store.AppendMessage(scope, state.Message{IsMe: true, MessageID: 777, Text: "answer"})

	addition := reactionUpdateWithID(101, 100500, 777, &tg.User{ID: 42, FirstName: "Alice"}, emojiReaction("👍"))
	removal := reactionUpdateWithID(102, 100500, 777, &tg.User{ID: 42, FirstName: "Alice"}, nil)
	removal.MessageReaction.OldReaction = emojiReaction("👍")

	// Deliver in reversed order: the removal is applied first.
	p.deliver(t, removal)
	p.deliver(t, addition)

	reactions := p.store.Snapshot(scope).Messages[0].Reactions
	if len(reactions) != 0 {
		t.Fatalf("stale addition must not resurrect the removed reaction, got %+v", reactions)
	}

	// And the normal order still works: a newer addition applies.
	newer := reactionUpdateWithID(103, 100500, 777, &tg.User{ID: 42, FirstName: "Alice"}, emojiReaction("🔥"))
	p.deliver(t, newer)

	reactions = p.store.Snapshot(scope).Messages[0].Reactions
	if len(reactions) != 1 || reactions[0].Emoji != "🔥" {
		t.Fatalf("newer reaction must apply, got %+v", reactions)
	}
}

// TestReactionFromNonWhitelistedChatDropped proves the reaction handler
// enforces the access gate itself, since reaction updates bypass the shared
// middlewares.
func TestReactionFromNonWhitelistedChatDropped(t *testing.T) {
	// Plain adminStoreStub defaults to allowlist policy with no whitelisted
	// chats, so every reaction must be denied.
	spy := &dmHistorySpy{}
	p := newReactionTestBot(t, &adminStoreStub{}, spy)

	p.deliver(t, reactionUpdate(100500, 777, &tg.User{ID: 42, FirstName: "Alice"}, emojiReaction("👍")))

	if got := spy.reactionCallCount(); got != 0 {
		t.Fatalf("non-whitelisted chat reaction must be dropped, got %d store calls", got)
	}
}

// TestReactionNonAdminPrivateDroppedWithDMAdminsOnly proves BOT__DM_ADMINS_ONLY
// covers reaction updates too: the middleware gate only inspects Message
// updates, so the handler re-enforces the restriction for private chats
// before the access policy is consulted.
func TestReactionNonAdminPrivateDroppedWithDMAdminsOnly(t *testing.T) {
	spy := &dmHistorySpy{}
	p := newReactionTestBot(t, &dmCatalogSpy{}, spy)
	p.bot.cfg.DMAdminsOnly = true

	// Non-admin private reaction: dropped even under the allow-all policy.
	p.deliver(t, reactionUpdate(100500, 777, &tg.User{ID: 42, FirstName: "Alice"}, emojiReaction("👍")))
	if got := spy.reactionCallCount(); got != 0 {
		t.Fatalf("non-admin private reaction must be dropped, got %d store calls", got)
	}

	// Admin private reactions keep flowing.
	p.deliver(t, reactionUpdateWithID(45, 100500, 777, &tg.User{ID: 7, FirstName: "Admin"}, emojiReaction("👍")))
	if got := spy.reactionCallCount(); got != 1 {
		t.Fatalf("admin private reaction must be processed, got %d store calls", got)
	}

	// Group reactions are unaffected by the DM restriction.
	groupUpdate := reactionUpdateWithID(46, -100999, 777, &tg.User{ID: 42, FirstName: "Alice"}, emojiReaction("👍"))
	groupUpdate.MessageReaction.Chat = tg.Chat{ID: -100999, Type: tg.ChatTypeSupergroup}
	p.deliver(t, groupUpdate)
	if got := spy.reactionCallCount(); got != 2 {
		t.Fatalf("group reaction must not be affected by the DM restriction, got %d store calls", got)
	}
}

// TestReactionHeldDuringSendWindowIsReplayedOnInsertion proves a reaction
// that arrives while the bot reply's send response is still in flight is
// buffered and applied once the reply is recorded in history.
func TestReactionHeldDuringSendWindowIsReplayedOnInsertion(t *testing.T) {
	p := newReactionTestBot(t, &dmCatalogSpy{}, newMemoryConversationStore())

	// The reply is already visible on Telegram (so users can react) before
	// its send response lands and history insertion happens: no target yet.
	p.deliver(t, reactionUpdateWithID(101, 100500, 777, &tg.User{ID: 42, FirstName: "Alice"}, emojiReaction("👍")))
	if got := p.store.Snapshot(state.ConversationScope{ChatID: 100500}).Messages; len(got) != 0 {
		t.Fatalf("nothing must be recorded before insertion, got %+v", got)
	}

	replyTo := tg.Message{MessageID: 55, Chat: tg.Chat{ID: 100500, Type: tg.ChatTypePrivate}}
	p.bot.saveBotReplyToHistory(context.Background(), replyTo, "answer", 777)

	snapshot := p.store.Snapshot(state.ConversationScope{ChatID: 100500})
	if len(snapshot.Messages) != 1 || len(snapshot.Messages[0].Reactions) != 1 || snapshot.Messages[0].Reactions[0].Emoji != "👍" {
		t.Fatalf("held reaction must be applied when the reply lands: %+v", snapshot.Messages)
	}
}

// TestReactionRemovalDuringSendCannotBeResurrected reproduces the send-window
// race end to end: the removal arrives before history insertion, the older
// addition after it, and the sequence guard must keep the reaction removed.
func TestReactionRemovalDuringSendCannotBeResurrected(t *testing.T) {
	p := newReactionTestBot(t, &dmCatalogSpy{}, newMemoryConversationStore())

	removal := reactionUpdateWithID(102, 100500, 777, &tg.User{ID: 42, FirstName: "Alice"}, nil)
	removal.MessageReaction.OldReaction = emojiReaction("👍")
	p.deliver(t, removal) // arrives during the send: held, nothing recorded

	replyTo := tg.Message{MessageID: 55, Chat: tg.Chat{ID: 100500, Type: tg.ChatTypePrivate}}
	p.bot.saveBotReplyToHistory(context.Background(), replyTo, "answer", 777) // replays update 102

	// The addition was delayed past the insertion: it is older than the
	// replayed removal and must stay rejected.
	p.deliver(t, reactionUpdateWithID(101, 100500, 777, &tg.User{ID: 42, FirstName: "Alice"}, emojiReaction("👍")))

	reactions := p.store.Snapshot(state.ConversationScope{ChatID: 100500}).Messages[0].Reactions
	if len(reactions) != 0 {
		t.Fatalf("delayed addition must not resurrect the removed reaction, got %+v", reactions)
	}
	if got := len(p.bot.pendingReactions.entries); got != 0 {
		t.Fatalf("every update must be replayed or applied live: %d holds stranded, %+v", got, p.bot.pendingReactions.entries)
	}
}

// TestReactionAppliedLiveLeavesNoHoldBehind proves the hold is released when
// the store lookup succeeds: only genuinely in-flight targets keep entries.
func TestReactionAppliedLiveLeavesNoHoldBehind(t *testing.T) {
	p := newReactionTestBot(t, &dmCatalogSpy{}, newMemoryConversationStore())

	scope := state.ConversationScope{ChatID: 100500}
	p.store.AppendMessage(scope, state.Message{IsMe: true, MessageID: 777, Text: "answer"})

	p.deliver(t, reactionUpdate(100500, 777, &tg.User{ID: 42, FirstName: "Alice"}, emojiReaction("👍")))

	snapshot := p.store.Snapshot(scope)
	if len(snapshot.Messages[0].Reactions) != 1 {
		t.Fatalf("reaction must be applied live, got %+v", snapshot.Messages[0].Reactions)
	}
	if got := len(p.bot.pendingReactions.entries); got != 0 {
		t.Fatalf("live-applied reaction must release its hold, got %d entries: %+v", got, p.bot.pendingReactions.entries)
	}
}

// TestReactionInGroupChatRecorded proves the same flow works in groups where
// the bot sees per-user reactions (bot admin required in production).
func TestReactionInGroupChatRecorded(t *testing.T) {
	p := newReactionTestBot(t, &dmCatalogSpy{}, newMemoryConversationStore())

	scope := state.ConversationScope{ChatID: -100999, TopicID: 5}
	p.store.AppendMessage(scope, state.Message{IsMe: true, MessageID: 777, Text: "answer"})

	update := reactionUpdate(-100999, 777, &tg.User{ID: 42, FirstName: "Alice"}, emojiReaction("👍"))
	update.MessageReaction.Chat = tg.Chat{ID: -100999, Type: tg.ChatTypeSupergroup, Title: "Test Group"}
	p.deliver(t, update)

	snapshot := p.store.Snapshot(scope)
	if len(snapshot.Messages[0].Reactions) != 1 || snapshot.Messages[0].Reactions[0].Emoji != "👍" {
		t.Fatalf("group reaction must be recorded on the bot message, got %+v", snapshot.Messages[0].Reactions)
	}
}

// TestRichReplyCapturesSentMessageID proves the reply send path keeps the
// sent message ID and stores it in history, so reactions can be mapped back.
func TestRichReplyCapturesSentMessageID(t *testing.T) {
	store := newMemoryConversationStore()
	api := newFakeAPIServer(t)
	botAPI, err := tg.NewBot("1:"+strings.Repeat("A", 35), tg.WithAPIServer(api.server.URL), tg.WithDiscardLogger())
	if err != nil {
		t.Fatalf("NewBot: %v", err)
	}

	testBot := &Bot{
		ctx:       context.Background(),
		logger:    reactionTestLogger(),
		history:   store,
		sanitizer: markdown.NewTgMarkdownV2Sanitizer(),
		api:       botAPI,
	}

	message := tg.Message{MessageID: 55, Chat: tg.Chat{ID: 100500, Type: tg.ChatTypePrivate}}
	sent, err := testBot.sendRichMarkdownReply(context.Background(), message, "the answer")
	if err != nil {
		t.Fatalf("sendRichMarkdownReply: %v", err)
	}
	if sent == nil || sent.MessageID == 0 {
		t.Fatalf("sent message with ID expected, got %+v", sent)
	}

	testBot.saveBotReplyToHistory(context.Background(), message, "the answer", sent.MessageID)

	scope := state.ConversationScope{ChatID: 100500}
	snapshot := store.Snapshot(scope)
	if len(snapshot.Messages) != 1 {
		t.Fatalf("expected one history entry, got %d", len(snapshot.Messages))
	}
	reply := snapshot.Messages[0]
	if !reply.IsMe || reply.MessageID != sent.MessageID {
		t.Fatalf("history entry must carry the sent message ID %d, got %+v", sent.MessageID, reply)
	}
}

// TestTrackedReactionEmojiExactTelegramForms proves the whitelist uses the
// exact strings Telegram transmits (no variation selectors): a mismatched
// entry would silently never match.
func TestTrackedReactionEmojiExactTelegramForms(t *testing.T) {
	if len(trackedReactionEmojis) == 0 {
		t.Fatalf("whitelist must not be empty")
	}

	for emoji := range trackedReactionEmojis {
		if emoji == "" {
			t.Fatalf("empty whitelist entry")
		}
		if strings.ContainsRune(emoji, '️') {
			t.Fatalf("whitelist entry %q contains a variation selector; Telegram sends plain codepoints", emoji)
		}
	}

	// The heart and the bolt are the classic mismatch cases.
	if _, ok := trackedReactionEmojis["❤"]; !ok {
		t.Fatalf("whitelist must track the plain heart U+2764 that Telegram sends")
	}
	if _, ok := trackedReactionEmojis["⚡"]; !ok {
		t.Fatalf("whitelist must track the plain bolt U+26A1 that Telegram sends")
	}
}
