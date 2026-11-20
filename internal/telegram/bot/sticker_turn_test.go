package bot

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"telegram-ollama-reply-bot/internal/adminconfig"
	"telegram-ollama-reply-bot/internal/interactivity"
	"telegram-ollama-reply-bot/internal/llm"
	"telegram-ollama-reply-bot/internal/stickers"

	tg "github.com/mymmrac/telego"
)

type testStickerCatalog struct{ index *stickers.Index }

func (c testStickerCatalog) Snapshot() *stickers.Index { return c.index }

func stickerSendBatch(callID string) llm.Response {
	return llm.Response{Message: llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{
		{ID: callID, Name: "send_sticker", Arguments: json.RawMessage(`{"id":"sticker_one"}`)},
	}}}
}

func stickerFinalText() llm.Response {
	return llm.Response{Message: llm.TextMessage(llm.RoleAssistant, "ok")}
}

// TestStickerCooldownFollowsTheTurnTrigger pins, through the real pipeline,
// that the trigger Telegram metadata yields (not the model) decides whether
// the sticker cooldown applies: a proactive turn right after a proactive
// sticker cannot send one, while an explicit mention in the same chat can.
func TestStickerCooldownFollowsTheTurnTrigger(t *testing.T) {
	p := newProactiveTestBot(t, func(s *adminStoreStub) {
		s.DefaultInteractivityMode = adminconfig.InteractivityMentionsReplies
		s.DefaultInteractionProfile = adminconfig.ProfileTalkLow
		s.SystemOneModel = "decision-model"
	})
	index := stickers.BuildIndex(stickers.State{
		Sets: []stickers.Set{{Name: "Kitsu", Title: "Kitsu", Enabled: true, Revision: 1}},
		Entries: []stickers.Entry{{
			SetName: "Kitsu", FileUniqueID: "u1", FileID: "file-1", SemanticID: "sticker_one", Description: "Happy fox",
			Emoji: "🦊", Format: stickers.FormatStatic, Position: 1, Available: true, Enabled: true,
		}},
	})
	p.bot.tools.AttachStickerCatalog(testStickerCatalog{index})

	engine := p.wireFakeEngine(t)
	engine.script = func(_ context.Context, _ interactivity.DecisionInput) (interactivity.ActionDecision, error) {
		return interactivity.ActionDecision{Action: interactivity.ActionReply, Reason: interactivity.ReasonReply}, nil
	}

	// Turn 1 (proactive): sends a sticker, then answers. Turn 2 (proactive,
	// inside the cooldown): the tool is no longer offered, so the scripted
	// call is rejected. Turn 3 (explicit mention): the same call goes through.
	p.llm.mu.Lock()
	p.llm.responses = []llm.Response{
		stickerSendBatch("one"), stickerFinalText(),
		stickerSendBatch("two"), stickerFinalText(),
		stickerSendBatch("three"), stickerFinalText(),
	}
	p.llm.mu.Unlock()

	proactive := func(messageID int, text string) tg.Update {
		update := proactiveGroupUpdate(text)
		update.UpdateID, update.Message.MessageID = 1000+messageID, 1000+messageID

		return update
	}

	p.deliver(t, proactive(1, "first proactive moment"))
	waitFor(t, func() bool { return p.api.callCount("sendSticker") == 1 && p.deliveredCount() >= 1 })

	p.deliver(t, proactive(2, "second proactive moment"))
	waitFor(t, func() bool { return p.deliveredCount() >= 2 })
	time.Sleep(100 * time.Millisecond) // settle: a wrongly admitted send would land by now
	if got := p.api.callCount("sendSticker"); got != 1 {
		t.Fatalf("a proactive turn inside the cooldown must not send a sticker, got %d sends", got)
	}

	p.deliver(t, proactiveMentionUpdate())
	waitFor(t, func() bool { return p.api.callCount("sendSticker") == 2 })
}
