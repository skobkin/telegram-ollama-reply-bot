package adminui

import (
	"encoding/json"
	"strings"
	"testing"

	tg "github.com/mymmrac/telego"

	core "telegram-ollama-reply-bot/internal/adminui"
)

const sharedGroupID = int64(-1001234567890)

func sharedGroupInfo() *tg.ChatFullInfo {
	return &tg.ChatFullInfo{
		ID:    sharedGroupID,
		Type:  "supergroup",
		Title: "Shared Group",
	}
}

// beginPicker opens the add-chat flow and returns the picker request ID.
func (h *harness) beginPicker(t *testing.T) int {
	t.Helper()

	h.open(t)
	h.tap(t, core.VerbCatalog, "a")

	session := h.manager.Session(testAdmin)
	if session == nil || session.Pending == nil || session.Pending.Kind != core.PendingChatPicker {
		t.Fatalf("add-chat must arm a chat picker pending")
	}

	return session.Pending.RequestID
}

func chatSharedUpdate(requestID int) tg.Update {
	return tg.Update{UpdateID: 45, Message: &tg.Message{
		MessageID: 52,
		Chat:      tg.Chat{ID: testAdmin, Type: tg.ChatTypePrivate},
		From:      &tg.User{ID: testAdmin},
		ChatShared: &tg.ChatShared{
			RequestID: requestID,
			ChatID:    sharedGroupID,
			Title:     "Shared Group",
		},
	}}
}

// TestPickerKeyboardIsUnconstrained proves the request-chat button sets no
// bot-membership or admin-rights constraints: chats the bot has never joined
// stay selectable, and channels are the only exclusion.
func TestPickerKeyboardIsUnconstrained(t *testing.T) {
	h := newHarness(t)
	requestID := h.beginPicker(t)

	sends := h.api.requestsOf("sendMessage")
	var pickerBody map[string]any
	if err := json.Unmarshal([]byte(sends[len(sends)-1].Body), &pickerBody); err != nil {
		t.Fatalf("unmarshal picker body: %v", err)
	}

	markup, _ := pickerBody["reply_markup"].(map[string]any)
	keyboard, _ := markup["keyboard"].([]any)
	if len(keyboard) != 1 {
		t.Fatalf("picker keyboard expected one row, got %v", markup)
	}
	row, _ := keyboard[0].([]any)
	if len(row) != 1 {
		t.Fatalf("picker keyboard expected one button, got %v", keyboard)
	}
	button, _ := row[0].(map[string]any)

	request, _ := button["request_chat"].(map[string]any)
	if request == nil {
		t.Fatalf("picker button must request a chat, got %v", button)
	}
	if id, _ := request["request_id"].(float64); int(id) != requestID {
		t.Fatalf("request id = %v, want %d", request["request_id"], requestID)
	}
	if isChannel, _ := request["chat_is_channel"].(bool); isChannel {
		t.Fatalf("channels must be excluded, got %v", request)
	}
	for _, forbidden := range []string{"bot_is_member", "request_administrator_rights", "request_title", "request_name"} {
		if _, present := request[forbidden]; present {
			t.Fatalf("picker request must not constrain %s: %v", forbidden, request)
		}
	}

	if text := lastJSONText(pickerBody); !strings.HasPrefix(text, core.AdminInputMarker) {
		t.Fatalf("picker prompt must carry the input marker, got %q", text)
	}
}

// TestChatSharedAddsWithVerification proves the full native selection flow:
// the reply keyboard is removed first, GetChat resolves real display data,
// the chat lands in the catalog and the allowlist, and the card opens.
func TestChatSharedAddsWithVerification(t *testing.T) {
	h := newHarness(t)
	requestID := h.beginPicker(t)
	h.api.chats[sharedGroupID] = sharedGroupInfo()

	h.deliver(t, chatSharedUpdate(requestID))

	// The reply keyboard disappears before the completion message.
	keyboardRemoval := h.api.lastBodyJSON("sendMessage")
	removal, _ := keyboardRemoval["reply_markup"].(map[string]any)
	_, removed := removal["remove_keyboard"]
	if !removed {
		t.Fatalf("the picker keyboard must be removed on selection, got %v", keyboardRemoval)
	}

	if got := h.api.callCount("getChat"); got != 1 {
		t.Fatalf("GetChat verification expected, got %d", got)
	}

	calls := h.store.writeCalls()
	if !containsCall(calls, "UpsertChatCatalog:-1001234567890") {
		t.Fatalf("catalog upsert missing, calls: %v", calls)
	}
	if !containsCall(calls, "AddWhitelistChat:-1001234567890") {
		t.Fatalf("allowlist add missing, calls: %v", calls)
	}

	card := h.api.lastSentText("editMessageText")
	if !strings.Contains(card, "Shared Group") || !strings.Contains(card, "Added to allowlist.") {
		t.Fatalf("verified chat card expected, got %q", card)
	}
}

// TestChatSharedWithoutGetChatAddsUnverified proves the graceful fallback:
// when the chat cannot be verified, a minimal entry is added and the card
// says so.
func TestChatSharedWithoutGetChatAddsUnverified(t *testing.T) {
	h := newHarness(t)
	requestID := h.beginPicker(t) // fake answers getChat with an error

	h.deliver(t, chatSharedUpdate(requestID))

	calls := h.store.writeCalls()
	if !containsCall(calls, "UpsertChatCatalog:-1001234567890") {
		t.Fatalf("catalog upsert missing, calls: %v", calls)
	}

	card := h.api.lastSentText("editMessageText")
	if !strings.Contains(card, "unverified") {
		t.Fatalf("unverified chat card expected, got %q", card)
	}
}

// TestDuplicateChatSharedIsConsumedSafely proves a second selection for an
// already-processed request never repeats the add nor falls through. After
// the completion finished, the pending is gone, so the duplicate is consumed
// with the inactive notice (the mid-verification window answering "already
// processed" is covered by the controller tests).
func TestDuplicateChatSharedIsConsumedSafely(t *testing.T) {
	h := newHarness(t)
	requestID := h.beginPicker(t)
	h.api.chats[sharedGroupID] = sharedGroupInfo()

	h.deliver(t, chatSharedUpdate(requestID))
	h.deliver(t, chatSharedUpdate(requestID))

	upserts := 0
	for _, call := range h.store.writeCalls() {
		if strings.Contains(call, "UpsertChatCatalog:") {
			upserts++
		}
	}
	if upserts != 1 {
		t.Fatalf("duplicate selection must not re-add, got %d upserts", upserts)
	}

	if text := h.api.lastSentText("sendMessage"); !strings.Contains(text, "no longer active") {
		t.Fatalf("duplicate selection notice expected, got %q", text)
	}
}

// TestManualChatIDViaPickerReply proves the numeric-reply fallback on the
// picker prompt: replying with a chat ID verifies and completes the add
// exactly like the native selection.
func TestManualChatIDViaPickerReply(t *testing.T) {
	h := newHarness(t)
	h.beginPicker(t)
	h.api.chats[sharedGroupID] = sharedGroupInfo()

	prompt := botPromptMessage(int(h.api.lastSendMessageID()), core.AdminInputMarker+" — Add chat")
	h.deliver(t, replyToBotUpdate("-1001234567890", prompt))

	if got := h.api.callCount("getChat"); got != 1 {
		t.Fatalf("manual ID must verify via GetChat, got %d", got)
	}

	calls := h.store.writeCalls()
	if !containsCall(calls, "AddWhitelistChat:-1001234567890") {
		t.Fatalf("allowlist add missing, calls: %v", calls)
	}
	if card := h.api.lastSentText("editMessageText"); !strings.Contains(card, "Added to allowlist.") {
		t.Fatalf("verified manual add expected, got %q", card)
	}
}

// TestManualChatIDInvalidInputRetries proves non-numeric replies to the
// picker prompt produce actionable feedback with attempt counting instead of
// silently consuming or leaking the input.
func TestManualChatIDInvalidInputRetries(t *testing.T) {
	h := newHarness(t)
	h.beginPicker(t)

	prompt := botPromptMessage(int(h.api.lastSendMessageID()), core.AdminInputMarker+" — Add chat")
	h.deliver(t, replyToBotUpdate("not a number", prompt))

	if text := h.api.lastSentText("sendMessage"); !strings.Contains(text, "numeric chat ID") ||
		!strings.Contains(text, "attempt 1 of 3") {
		t.Fatalf("retry feedback expected, got %q", text)
	}
	if got := h.api.callCount("getChat"); got != 0 {
		t.Fatalf("invalid input must not trigger verification, got %d", got)
	}
	if calls := h.store.writeCalls(); len(calls) != 0 {
		t.Fatalf("invalid input must not write, got %v", calls)
	}
}

// TestChatSharedWrongRequestIDConsumed proves a selection for a different
// request (or after expiry) is consumed with a notice and never reaches the
// conversation pipeline.
func TestChatSharedWrongRequestIDConsumed(t *testing.T) {
	h := newHarness(t)
	h.beginPicker(t)

	h.deliver(t, chatSharedUpdate(requestIDOther()))

	if text := h.api.lastSentText("sendMessage"); !strings.Contains(text, "no longer active") {
		t.Fatalf("inactive selection notice expected, got %q", text)
	}
	for _, call := range h.store.writeCalls() {
		if strings.Contains(call, "UpsertChatCatalog") || strings.Contains(call, "AddWhitelistChat") {
			t.Fatalf("inactive selection must not write, got %v", call)
		}
	}
}

func requestIDOther() int { return 424242 }

func containsCall(calls []string, want string) bool {
	for _, call := range calls {
		if strings.Contains(call, want) {
			return true
		}
	}

	return false
}
