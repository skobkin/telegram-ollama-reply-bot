package bot

import (
	"context"
	"testing"
	"time"

	"telegram-ollama-reply-bot/internal/state"

	tg "github.com/mymmrac/telego"
)

const attachmentChatID = int64(100500)

func newAttachmentTestBot(t *testing.T) *reactionTestBot {
	t.Helper()

	p := newReactionTestBot(t, &dmCatalogSpy{}, newMemoryConversationStore())
	p.bot.me = testBotID

	return p
}

func botPollAttachment() *state.Attachment {
	return &state.Attachment{Kind: state.AttachmentPoll, Label: `"Dinner?" — Pizza / Sushi`}
}

func userMessage(extra func(*tg.Message)) tg.Message {
	msg := tg.Message{
		MessageID: 300,
		Date:      1700000000,
		Chat:      tg.Chat{ID: attachmentChatID, Type: tg.ChatTypePrivate},
		From:      &tg.User{ID: 42, FirstName: "Alice", Username: "alice"},
	}
	extra(&msg)

	return msg
}

// Messages without text are no longer empty history entries: their non-text
// content is kept as a short description, in the message's own scope.
func TestIncomingNonTextMessagesGetAnAttachmentDescription(t *testing.T) {
	p := newAttachmentTestBot(t)

	tests := []struct {
		name     string
		msg      tg.Message
		wantKind state.AttachmentKind
		want     string
	}{
		{"sticker", userMessage(func(m *tg.Message) { m.Sticker = &tg.Sticker{Emoji: "😀", SetName: "catpack"} }), state.AttachmentSticker, "😀 (catpack)"},
		{"voice", userMessage(func(m *tg.Message) { m.Voice = &tg.Voice{Duration: 12} }), state.AttachmentVoice, "0:12"},
		{"poll", userMessage(func(m *tg.Message) {
			m.Poll = &tg.Poll{Question: "Tea or coffee?", Options: []tg.PollOption{{Text: "Tea"}, {Text: "Coffee"}}}
		}), state.AttachmentPoll, `"Tea or coffee?" — Tea / Coffee`},
		{"document with caption keeps both", userMessage(func(m *tg.Message) {
			m.Document = &tg.Document{FileName: "report.pdf"}
			m.Caption = "the quarterly report"
		}), state.AttachmentDocument, "report.pdf"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := p.bot.tgUserMessageToMessageData(tt.msg, false)
			if got.Attachment == nil || got.Attachment.Kind != tt.wantKind || got.Attachment.Label != tt.want {
				t.Fatalf("attachment = %+v, want {%s %q}", got.Attachment, tt.wantKind, tt.want)
			}
			if got.HasImage || got.ImageMeta != nil {
				t.Fatalf("non-photo content must not create image data: %+v", got)
			}
		})
	}

	document := p.bot.tgUserMessageToMessageData(tests[3].msg, false)
	if document.Text != "the quarterly report" {
		t.Fatalf("the caption must stay as the text: %q", document.Text)
	}
}

func TestIncomingTextAndPhotosGetNoAttachment(t *testing.T) {
	p := newAttachmentTestBot(t)

	text := p.bot.tgUserMessageToMessageData(userMessage(func(m *tg.Message) { m.Text = "hello" }), false)
	photo := p.bot.tgUserMessageToMessageData(userMessage(func(m *tg.Message) {
		m.Photo = []tg.PhotoSize{{FileID: "f", FileUniqueID: "u", Width: 10, Height: 10}}
	}), false)

	if text.Attachment != nil || photo.Attachment != nil {
		t.Fatalf("plain text and photos keep their own representations: text=%+v photo=%+v", text.Attachment, photo.Attachment)
	}
	if !photo.HasImage || photo.ImageMeta == nil {
		t.Fatalf("photos must keep the image path: %+v", photo)
	}
}

func TestRecordBotMessageStampsAuthorshipScopeAndTime(t *testing.T) {
	p := newAttachmentTestBot(t)
	scope := state.ConversationScope{ChatID: attachmentChatID, TopicID: 9}

	before := time.Now().UTC().Add(-time.Second)
	p.bot.RecordBotMessage(context.Background(), scope, state.Message{MessageID: 777, Attachment: botPollAttachment()})

	messages := p.store.Snapshot(scope).Messages
	if len(messages) != 1 {
		t.Fatalf("expected one recorded message, got %+v", messages)
	}
	got := messages[0]
	if !got.IsMe || got.Name != "TestBot" || got.Username != "testbot" {
		t.Fatalf("bot authorship missing: %+v", got)
	}
	if got.ChatID != scope.ChatID || got.TopicID != scope.TopicID || got.MessageID != 777 {
		t.Fatalf("scope/id not stamped: %+v", got)
	}
	if got.CreatedAt.Before(before) {
		t.Fatalf("missing timestamps must default to now, got %v", got.CreatedAt)
	}
	if got.Attachment == nil || got.Attachment.Kind != state.AttachmentPoll {
		t.Fatalf("attachment lost: %+v", got)
	}

	// Telegram's own timestamp is kept when the caller has one.
	stamp := time.Unix(1700000000, 0).UTC()
	p.bot.RecordBotMessage(context.Background(), scope, state.Message{MessageID: 778, CreatedAt: stamp, Attachment: botPollAttachment()})
	if last := p.store.Snapshot(scope).Messages[1]; !last.CreatedAt.Equal(stamp) {
		t.Fatalf("explicit timestamp overwritten: %v", last.CreatedAt)
	}
}

// Users can react to a bot poll or sticker, and the reaction may arrive
// before the send is recorded: the recorder replays it like for text replies.
func TestRecordBotMessageReplaysReactionsHeldDuringTheSend(t *testing.T) {
	p := newAttachmentTestBot(t)
	scope := state.ConversationScope{ChatID: attachmentChatID}

	p.deliver(t, reactionUpdateWithID(101, attachmentChatID, 777, &tg.User{ID: 42, FirstName: "Alice"}, emojiReaction("🔥")))
	if got := p.store.Snapshot(scope).Messages; len(got) != 0 {
		t.Fatalf("nothing must be recorded before insertion, got %+v", got)
	}

	p.bot.RecordBotMessage(context.Background(), scope, state.Message{MessageID: 777, Attachment: botPollAttachment()})

	messages := p.store.Snapshot(scope).Messages
	if len(messages) != 1 || len(messages[0].Reactions) != 1 || messages[0].Reactions[0].Emoji != "🔥" {
		t.Fatalf("held reaction must be applied to the recorded media: %+v", messages)
	}

	// And a reaction after recording applies live.
	p.deliver(t, reactionUpdateWithID(102, attachmentChatID, 777, &tg.User{ID: 43, FirstName: "Bob"}, emojiReaction("👍")))
	if got := len(p.store.Snapshot(scope).Messages[0].Reactions); got != 2 {
		t.Fatalf("a live reaction on recorded media must be stored, got %d reactions", got)
	}
}

func quotedBotMessage(extra func(*tg.Message)) *tg.Message {
	msg := &tg.Message{
		MessageID: 777,
		Date:      1700000000,
		Chat:      tg.Chat{ID: attachmentChatID, Type: tg.ChatTypePrivate},
		From:      &tg.User{ID: testBotID.ID, FirstName: testBotID.FirstName, Username: testBotID.Username, IsBot: true},
	}
	extra(msg)

	return msg
}

// A user replying to a bot poll resolves to the stored entry, so the reply
// keeps its relationship and the model sees what was replied to.
func TestReplyToRecordedBotPollResolvesTheStoredEntry(t *testing.T) {
	p := newAttachmentTestBot(t)
	scope := state.ConversationScope{ChatID: attachmentChatID}
	p.bot.RecordBotMessage(context.Background(), scope, state.Message{MessageID: 777, Attachment: botPollAttachment()})

	reply := userMessage(func(m *tg.Message) {
		m.Text = "pizza!"
		m.ReplyToMessage = quotedBotMessage(func(q *tg.Message) { q.Poll = &tg.Poll{Question: "Dinner?"} })
	})
	got := p.bot.tgUserMessageToMessageData(reply, true)

	if got.ReplyTo == nil {
		t.Fatal("a reply to a bot poll must keep its reply link")
	}
	if got.ReplyTo.MessageID != 777 || !got.ReplyTo.IsMe || got.ReplyTo.Attachment == nil || got.ReplyTo.Attachment.Label != botPollAttachment().Label {
		t.Fatalf("reply target must be the stored bot poll: %+v", got.ReplyTo)
	}
}

// With no stored copy (restart, eviction) a quoted bot sticker still carries
// its description instead of being dropped for lacking text.
func TestReplyToUnstoredBotStickerKeepsItsDescription(t *testing.T) {
	p := newAttachmentTestBot(t)

	reply := userMessage(func(m *tg.Message) {
		m.Text = "lol"
		m.ReplyToMessage = quotedBotMessage(func(q *tg.Message) { q.Sticker = &tg.Sticker{Emoji: "😂", SetName: "foxes"} })
	})
	got := p.bot.tgUserMessageToMessageData(reply, true)

	if got.ReplyTo == nil || !got.ReplyTo.IsMe {
		t.Fatalf("the quote must survive as a bot message: %+v", got.ReplyTo)
	}
	if got.ReplyTo.Attachment == nil || got.ReplyTo.Attachment.Kind != state.AttachmentSticker || got.ReplyTo.Attachment.Label != "😂 (foxes)" {
		t.Fatalf("attachment = %+v", got.ReplyTo.Attachment)
	}
}

// Quotes with neither text nor a describable attachment still carry nothing.
func TestReplyToContentlessBotMessageIsDropped(t *testing.T) {
	p := newAttachmentTestBot(t)

	reply := userMessage(func(m *tg.Message) {
		m.Text = "?"
		m.ReplyToMessage = quotedBotMessage(func(*tg.Message) {})
	})
	if got := p.bot.tgUserMessageToMessageData(reply, true); got.ReplyTo != nil {
		t.Fatalf("an empty quote must not enter context: %+v", got.ReplyTo)
	}
}
