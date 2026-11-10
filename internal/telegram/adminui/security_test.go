package adminui

import (
	"strings"
	"testing"

	tg "github.com/mymmrac/telego"

	core "telegram-ollama-reply-bot/internal/adminui"
)

const otherAdminID = int64(8)

// TestForgedCallbackArgumentsNeverWrite proves well-formed callbacks with
// hostile arguments (bogus enum value, nonexistent chat, unknown feature)
// are answered exactly once and never reach the store.
func TestForgedCallbackArgumentsNeverWrite(t *testing.T) {
	forged := []string{
		core.EncodeAction(core.VerbValue, 1, "im", "proactive"), // unsupported enum
		core.EncodeAction(core.VerbChat, 1, "999999999"),        // nonexistent chat
		core.EncodeAction(core.VerbPromptFeat, 1, "zz"),         // unknown feature
		core.EncodeAction(core.VerbAccessPol, 1, "anarchy"),     // unknown policy
	}

	for _, data := range forged {
		h := newHarness(t)
		h.open(t)
		writesBefore := len(h.store.writeCalls())

		session := h.manager.Session(testAdmin)
		_, _, msgID, _ := session.DetachedSnapshot()
		h.deliver(t, callbackUpdate(testAdmin, msgID, data))

		if got := h.api.callCount("answerCallbackQuery"); got != 1 {
			t.Fatalf("%s: exactly one answer, got %d", data, got)
		}
		if writes := h.store.writeCalls(); len(writes) != writesBefore {
			t.Fatalf("%s: forged arguments must never write, got %v", data, writes)
		}
	}
}

// TestReplayedMutationCallbackAppliesExactlyOnce proves a mutation callback
// replayed byte-for-byte cannot repeat the write: the sequence guard makes
// the second delivery stale.
func TestReplayedMutationCallbackAppliesExactlyOnce(t *testing.T) {
	h := newHarness(t)
	h.open(t)
	h.tap(t, core.VerbGlobalSet)
	h.tap(t, core.VerbField, "im")

	session := h.manager.Session(testAdmin)
	_, _, msgID, _ := session.DetachedSnapshot()
	data := core.EncodeAction(core.VerbValue, session.Seq, "im", "mentions_only")

	h.deliver(t, callbackUpdate(testAdmin, msgID, data))
	h.deliver(t, callbackUpdate(testAdmin, msgID, data)) // byte-for-byte replay

	writes := 0
	for _, call := range h.store.writeCalls() {
		if strings.Contains(call, "default_interactivity_mode") {
			writes++
		}
	}
	if writes != 1 {
		t.Fatalf("replayed callback must apply exactly once, got %d writes", writes)
	}
}

// TestCrossAdminCallbackCannotMutateOtherSession proves one admin's button
// data cannot drive another admin's session: encoded against admin 7's
// sequence and pressed as admin 8 on 8's own live menu, the action lands as
// stale — a notice, never a mutation.
func TestCrossAdminCallbackCannotMutateOtherSession(t *testing.T) {
	h := newHarnessWithAdmins(t, testAdmin, otherAdminID)

	h.open(t) // admin 7's session
	h.deliver(t, privateUpdate(otherAdminID, "/admin"))

	admin7 := h.manager.Session(testAdmin)
	_, _, msgID8, _ := h.manager.Session(otherAdminID).DetachedSnapshot()

	// Data carries 7's sequence; the origin is 8's own menu message.
	h.deliver(t, callbackUpdate(otherAdminID, msgID8, core.EncodeAction(core.VerbValue, admin7.Seq, "im", "mentions_only")))

	if got := h.api.callCount("answerCallbackQuery"); got != 1 {
		t.Fatalf("exactly one answer, got %d", got)
	}
	for _, call := range h.store.writeCalls() {
		if strings.Contains(call, "SetGlobalField") || strings.Contains(call, "SetChatField") {
			t.Fatalf("cross-admin callback must never mutate, got %v", call)
		}
	}
}

// TestCallbackOnInaccessibleMessageRejected proves callbacks carried by an
// inaccessible message (Telegram strips old message content) answer
// menu-gone without processing.
func TestCallbackOnInaccessibleMessageRejected(t *testing.T) {
	h := newHarness(t)
	h.open(t)

	update := tg.Update{UpdateID: 47, CallbackQuery: &tg.CallbackQuery{
		ID:   "cb-2",
		From: tg.User{ID: testAdmin},
		Message: &tg.InaccessibleMessage{
			MessageID: 90,
			Chat:      tg.Chat{ID: testAdmin, Type: tg.ChatTypePrivate},
		},
		Data: core.EncodeAction(core.VerbHome, 1),
	}}
	h.deliver(t, update)

	if got := h.api.callCount("answerCallbackQuery"); got != 1 {
		t.Fatalf("exactly one answer, got %d", got)
	}
	if text := h.api.lastSentText("answerCallbackQuery"); text != core.AnswerMenuGone {
		t.Fatalf("inaccessible origin must answer menu-gone, got %q", text)
	}
	if got := h.api.callCount("editMessageText"); got != 0 {
		t.Fatalf("inaccessible origin must not edit, got %d", got)
	}
}

// TestReplyAfterCancelConsumedSafely proves the anti-leak path after an
// explicit /cancel: the reply is consumed with a notice, not the LLM, and
// writes nothing.
func TestReplyAfterCancelConsumedSafely(t *testing.T) {
	h := newHarness(t)
	h.open(t)
	h.tap(t, core.VerbGlobalSet)
	h.tap(t, core.VerbField, "lg")
	h.tap(t, core.VerbString, "lg")

	prompt := botPromptMessage(int(h.api.lastSendMessageID()), core.AdminInputMarker+" — Language")

	h.deliver(t, privateUpdate(testAdmin, "/cancel"))
	h.deliver(t, replyToBotUpdate("Русский", prompt))

	if text := h.api.lastSentText("sendMessage"); !strings.Contains(text, "no longer active") {
		t.Fatalf("cancelled input reply must be consumed with a notice, got %q", text)
	}
	if calls := h.store.writeCalls(); len(calls) != 0 {
		t.Fatalf("cancelled input reply must not write, got %v", calls)
	}
}

// TestOtherAdminReplyNeverTouchesArmedPending proves a reply by a different
// admin (even to a marker-prefixed bot message) only ever consults their own
// session: the first admin's pending survives untouched.
func TestOtherAdminReplyNeverTouchesArmedPending(t *testing.T) {
	h := newHarnessWithAdmins(t, testAdmin, otherAdminID)

	h.open(t)
	h.tap(t, core.VerbGlobalSet)
	h.tap(t, core.VerbField, "lg")
	h.tap(t, core.VerbString, "lg")

	pendingBefore := h.manager.Session(testAdmin).Pending
	if pendingBefore == nil {
		t.Fatalf("admin 7 must have an armed pending")
	}
	promptMsgID := pendingBefore.PromptMsgID

	// Admin 8 replies to a marker-looking bot message in their own chat.
	foreignPrompt := botPromptMessage(300, core.AdminInputMarker+" — Language")
	h.deliver(t, replyFrom(otherAdminID, "Русский", foreignPrompt))

	if text := h.api.lastSentText("sendMessage"); !strings.Contains(text, "no longer active") {
		t.Fatalf("foreign reply must be consumed with a notice, got %q", text)
	}

	pendingAfter := h.manager.Session(testAdmin).Pending
	if pendingAfter == nil || pendingAfter.PromptMsgID != promptMsgID {
		t.Fatalf("admin 7's pending must survive the foreign reply")
	}
	for _, call := range h.store.writeCalls() {
		if strings.Contains(call, "SetGlobalField") {
			t.Fatalf("foreign reply must never mutate, got %v", call)
		}
	}

	// The real owner's reply still processes normally.
	h.deliver(t, replyToBotUpdate("Русский", botPromptMessage(promptMsgID, core.AdminInputMarker+" — Language")))

	found := false
	for _, call := range h.store.writeCalls() {
		if strings.Contains(call, "SetGlobalField:language=Русский") {
			found = true
		}
	}
	if !found {
		t.Fatalf("the owner's reply must still process, calls: %v", h.store.writeCalls())
	}
}

// replyFrom builds a private-chat reply by a specific admin. All test DMs
// share the fake chat, so the from user is what distinguishes them.
func replyFrom(fromID int64, text string, replyTo tg.Message) tg.Update {
	update := replyToBotUpdate(text, replyTo)
	update.Message.From = &tg.User{ID: fromID}

	return update
}

// TestReplacedPendingPromptNeverCorrelates pins the pending-identity guard
// end to end: after the admin arms a second input (replacing the first
// pending), replying to the FIRST prompt must never write the second
// field — the reply is consumed as inactive instead.
func TestReplacedPendingPromptNeverCorrelates(t *testing.T) {
	h := newHarness(t)
	h.open(t)
	h.tap(t, core.VerbGlobalSet)

	// First input: character name.
	h.tap(t, core.VerbField, "cn")
	h.tap(t, core.VerbString, "cn")
	firstPromptMsgID := int(h.api.lastSendMessageID())
	firstPrompt := botPromptMessage(firstPromptMsgID, core.AdminInputMarker+" — Character name")

	// Second input replaces the pending before any reply arrives.
	h.tap(t, core.VerbBack)
	h.tap(t, core.VerbBack)
	h.tap(t, core.VerbField, "lg")
	h.tap(t, core.VerbString, "lg")
	if pending := h.manager.Session(testAdmin).Pending; pending == nil || pending.Field != "language" {
		t.Fatalf("the language pending must be live, got %+v", pending)
	}
	secondPromptMsgID := int(h.api.lastSendMessageID())

	h.deliver(t, replyToBotUpdate("Alice", firstPrompt))

	for _, call := range h.store.writeCalls() {
		if strings.Contains(call, "SetGlobalField:character_name") || strings.Contains(call, "SetGlobalField:language") {
			t.Fatalf("reply to a replaced pending's prompt must never write, got %v", call)
		}
	}
	if text := h.api.lastSentText("sendMessage"); !strings.Contains(text, "no longer active") {
		t.Fatalf("the orphaned reply must be consumed with a notice, got %q", text)
	}

	// The live pending still correlates only to its own prompt.
	secondPrompt := botPromptMessage(secondPromptMsgID, core.AdminInputMarker+" — Language")
	h.deliver(t, replyToBotUpdate("Русский", secondPrompt))
	found := false
	for _, call := range h.store.writeCalls() {
		if strings.Contains(call, "SetGlobalField:language=Русский") {
			found = true
		}
	}
	if !found {
		t.Fatalf("the live pending must still process its own reply, calls: %v", h.store.writeCalls())
	}
}
