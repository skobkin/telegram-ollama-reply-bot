package bot

import (
	"context"
	"sync"
	"testing"

	"telegram-ollama-reply-bot/internal/state"

	tg "github.com/mymmrac/telego"
)

// The reminder scheduler (and tools) can record bot messages before Run has
// resolved the identity itself: app wiring seeds it with SetIdentity, and the
// recorded entry must carry that name, not an empty one.
func TestRecordBotMessageUsesIdentitySeededBeforeRun(t *testing.T) {
	p := newAttachmentTestBot(t)
	p.bot.me = botInfo{} // Run has not resolved the identity yet
	scope := state.ConversationScope{ChatID: attachmentChatID}

	p.bot.SetIdentity(&tg.User{ID: 999, FirstName: "Kitsu", LastName: "Bot", Username: "kitsubot", IsBot: true})
	p.bot.RecordBotMessage(context.Background(), scope, state.Message{Text: "Reminder: water plants", MessageID: 31})

	messages := p.store.Snapshot(scope).Messages
	if len(messages) != 1 {
		t.Fatalf("expected one recorded message, got %+v", messages)
	}
	if got := messages[0]; !got.IsMe || got.Name != "Kitsu Bot" || got.Username != "kitsubot" {
		t.Fatalf("recorded entry must carry the seeded bot identity: %+v", got)
	}
}

// SetIdentity (refreshed by Run) and the recorders run on different
// goroutines; the identity they share must be synchronized. Under -race an
// unguarded b.me fails this test; without -race it still checks that every
// entry carries the identity.
func TestRecordBotMessageIdentityIsSafeAgainstConcurrentRefresh(t *testing.T) {
	p := newAttachmentTestBot(t)
	scope := state.ConversationScope{ChatID: attachmentChatID}
	user := &tg.User{ID: 999, FirstName: "TestBot", Username: "testbot", IsBot: true}
	p.bot.SetIdentity(user)

	const rounds = 50
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			p.bot.SetIdentity(user)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			p.bot.RecordBotMessage(context.Background(), scope, state.Message{Text: "tick", MessageID: 1000 + i})
		}
	}()
	wg.Wait()

	messages := p.store.Snapshot(scope).Messages
	if len(messages) != rounds {
		t.Fatalf("recorded %d messages, want %d", len(messages), rounds)
	}
	for _, msg := range messages {
		if msg.Name != "TestBot" || msg.Username != "testbot" {
			t.Fatalf("every entry must carry the bot identity: %+v", msg)
		}
	}
}
