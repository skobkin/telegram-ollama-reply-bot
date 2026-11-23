package tooluse

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"telegram-ollama-reply-bot/internal/llm"
	"telegram-ollama-reply-bot/internal/state"

	t "github.com/mymmrac/telego"
	"github.com/mymmrac/telego/telegoapi"
)

func stickerCatalogForSend(tst *testing.T) staticCatalog {
	tst.Helper()

	return staticCatalog{stickerIndex(tst, "Kitsu pack",
		stickerEntry(1, "Happy fox waving hello", "happy"),
		stickerEntry(2, "Sad fox crying", "sad"),
	)}
}

// directSticker builds a runtime for direct handler calls with an injectable
// clock. The returned clock pointer is advanced by tests.
func directSticker(tst *testing.T, sender *stubActionSender) (*Runtime, *time.Time) {
	tst.Helper()
	r := stickerRuntime(stickerCatalogForSend(tst).index)
	r.actions = sender
	clock := stickerTestNow
	r.stickerNow = func() time.Time { return clock }

	return r, &clock
}

func stickerCall(chat int64, topic int, proactive bool) CallContext {
	return CallContext{Scope: state.ConversationScope{ChatID: chat, TopicID: topic}, run: &runState{proactive: proactive}}
}

func sendStickerID(tst *testing.T, r *Runtime, call CallContext, id string) toolResult {
	tst.Helper()
	result, err := r.sendSticker(context.Background(), call, json.RawMessage(`{"id":"`+id+`"}`))
	if err != nil {
		tst.Fatalf("sendSticker(%q): %v", id, err)
	}

	return result
}

func sentOK() *stubActionSender { return &stubActionSender{message: &t.Message{MessageID: 77}} }

func TestSendStickerRoutesToTheCurrentChatAndTopicOnly(tst *testing.T) {
	sender := sentOK()
	r, _ := directSticker(tst, sender)

	result := sendStickerID(tst, r, stickerCall(-1001, 9, false), "sticker_01")
	if result.Status != "ok" {
		tst.Fatalf("unexpected failure: %+v", result.Error)
	}
	calls := sender.stickerCalls()
	if len(calls) != 1 {
		tst.Fatalf("expected one send, got %d", len(calls))
	}
	params := calls[0]
	if params.ChatID.ID != -1001 || params.MessageThreadID != 9 {
		tst.Fatalf("chat/topic must come from the run scope: %+v", params)
	}
	if params.Sticker.FileID != "SECRET-FILE-ID-01" || params.Sticker.URL != "" || params.Sticker.File != nil {
		tst.Fatalf("the send token must be the cataloged file_id: %+v", params.Sticker)
	}
	data := result.Data.(map[string]any)
	if data["message_id"] != 77 || data["id"] != "sticker_01" || data["chat_id"] != int64(-1001) || data["topic_id"] != 9 {
		tst.Fatalf("unexpected result data: %+v", data)
	}
	if wire, preparation := prepareResult(result, sendStickerResultCharBudget); preparation != ResultPreparationComplete || utf8.RuneCountInString(wire) > sendStickerResultCharBudget {
		tst.Fatalf("result must fit its budget: %s %s", preparation, wire)
	}

	// No topic: the thread parameter stays unset.
	sender2 := sentOK()
	r2, _ := directSticker(tst, sender2)
	sendStickerID(tst, r2, stickerCall(5, 0, false), "sticker_02")
	if got := sender2.stickerCalls()[0]; got.MessageThreadID != 0 || got.ChatID.ID != 5 {
		tst.Fatalf("no topic must mean no thread id: %+v", got)
	}
}

func TestSendStickerAcceptsOnlyACatalogID(tst *testing.T) {
	sender := sentOK()
	r, _ := directSticker(tst, sender)

	for _, bad := range []string{`{}`, `{"id":""}`, `{"id":"sticker_01","chat_id":5}`, `{"file_id":"AAA"}`, `{"id":5}`, `[]`} {
		if _, err := r.sendSticker(context.Background(), stickerCall(5, 0, false), json.RawMessage(bad)); err == nil {
			tst.Errorf("%s must be rejected as invalid arguments", bad)
		}
	}
	for _, id := range []string{"AAEAAgADBQAD", "https://example.com/s.webp", "Sticker_01", "no_such_sticker"} {
		result := sendStickerID(tst, r, stickerCall(5, 0, false), id)
		if result.Error == nil || result.Error.Code != "unknown_sticker" || result.Error.Class != ErrorNotFound || !result.Error.Retryable {
			tst.Errorf("%q must be an unknown_sticker miss: %+v", id, result.Error)
		}
	}
	if len(sender.stickerCalls()) != 0 {
		tst.Fatalf("nothing but a cataloged id may ever be dispatched")
	}
}

func TestSendStickerUnknownIDsNeverConsumeTheAttemptButAreBounded(tst *testing.T) {
	sender := sentOK()
	r, _ := directSticker(tst, sender)
	call := stickerCall(5, 0, false)

	sendStickerID(tst, r, call, "nope_one")
	sendStickerID(tst, r, call, "nope_two")
	if call.run.stickerAttempted || !call.run.canSendSticker() {
		tst.Fatalf("misses must not consume the send attempt")
	}
	if result := sendStickerID(tst, r, call, "sticker_01"); result.Status != "ok" {
		tst.Fatalf("a valid id after two misses must still send: %+v", result.Error)
	}

	blocked := stickerCall(5, 0, false)
	for i := 0; i < stickerMissLimit; i++ {
		sendStickerID(tst, r, blocked, "nope_id")
	}
	if blocked.run.canSendSticker() {
		tst.Fatalf("%d misses must block the tool for the turn", stickerMissLimit)
	}
	result := sendStickerID(tst, r, blocked, "sticker_01")
	if result.Error == nil || result.Error.Code != "sticker_lookup_limit" || result.Error.Retryable {
		tst.Fatalf("a blocked turn must not send even a valid id: %+v", result.Error)
	}
	if got := len(sender.stickerCalls()); got != 1 {
		tst.Fatalf("only the first turn's valid send may dispatch, got %d", got)
	}
}

func TestSendStickerOutcomes(tst *testing.T) {
	cases := map[string]struct {
		sender   *stubActionSender
		wantCode string
		class    string
	}{
		"bad request is a definite rejection":   {&stubActionSender{stickerErr: &telegoapi.Error{ErrorCode: 400, Description: "Bad Request: wrong file identifier"}}, "telegram_rejected", ErrorPermanent},
		"forbidden is a definite rejection":     {&stubActionSender{stickerErr: &telegoapi.Error{ErrorCode: 403, Description: "Forbidden: bot was kicked"}}, "telegram_rejected", ErrorPermanent},
		"rate limit":                            {&stubActionSender{stickerErr: &telegoapi.Error{ErrorCode: 429, Parameters: &telegoapi.ResponseParameters{RetryAfter: 5}}}, "rate_limited", ErrorRateLimited},
		"transport failure is ambiguous":        {&stubActionSender{stickerErr: errors.New("connection reset")}, "outcome_unknown", ErrorAmbiguous},
		"cancel during the send is ambiguous":   {&stubActionSender{stickerErr: context.Canceled}, "outcome_unknown", ErrorAmbiguous},
		"nil message is not a confirmation":     {&stubActionSender{}, "invalid_send_response", ErrorAmbiguous},
		"id-less message is not a confirmation": {&stubActionSender{message: &t.Message{}}, "invalid_send_response", ErrorAmbiguous},
	}
	for name, tc := range cases {
		tst.Run(name, func(tst *testing.T) {
			r, _ := directSticker(tst, tc.sender)
			call := stickerCall(5, 0, false)

			result := sendStickerID(tst, r, call, "sticker_01")
			if result.Status != "error" || result.Error == nil || result.Error.Code != tc.wantCode || result.Error.Class != tc.class {
				tst.Fatalf("unexpected result: %+v", result.Error)
			}
			if result.Error.Retryable {
				tst.Fatalf("a dispatched send is never retryable within the turn: %+v", result.Error)
			}
			if !call.run.stickerAttempted {
				tst.Fatalf("a dispatched attempt must use up the turn's send")
			}
			if again := sendStickerID(tst, r, call, "sticker_01"); again.Error == nil || again.Error.Code != "sticker_limit_reached" {
				tst.Fatalf("a failed or ambiguous send cannot be worked around: %+v", again)
			}
			if got := len(tc.sender.stickerCalls()); got != 1 {
				tst.Fatalf("exactly one dispatch attempt, got %d", got)
			}
		})
	}
}

func TestSendStickerPreDispatchCancellationIsDefinite(tst *testing.T) {
	sender := sentOK()
	r, _ := directSticker(tst, sender)
	call := stickerCall(5, 0, false)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, err := r.sendSticker(ctx, call, json.RawMessage(`{"id":"sticker_01"}`))
	if err != nil || result.Error == nil || result.Error.Code != "cancelled" || result.Error.Class != ErrorCancelled || result.Error.Retryable {
		tst.Fatalf("expected a definite cancellation: %+v %v", result.Error, err)
	}
	if len(sender.stickerCalls()) != 0 || call.run.stickerAttempted {
		tst.Fatalf("a cancelled call must neither dispatch nor consume the attempt")
	}
}

func TestStickerCooldownAppliesOnlyToProactiveTurns(tst *testing.T) {
	sender := sentOK()
	r, clock := directSticker(tst, sender)

	if result := sendStickerID(tst, r, stickerCall(5, 0, true), "sticker_01"); result.Status != "ok" {
		tst.Fatalf("the first proactive send must pass: %+v", result.Error)
	}

	*clock = clock.Add(10 * time.Second)
	declined := sendStickerID(tst, r, stickerCall(5, 0, true), "sticker_01")
	if declined.Error == nil || declined.Error.Code != "sticker_cooldown" || declined.Error.Class != ErrorRateLimited || declined.Error.Retryable {
		tst.Fatalf("a proactive send inside the window must be declined, non-retryably: %+v", declined.Error)
	}
	if declined.Error.RetryAfterSeconds != 20 {
		tst.Fatalf("the remaining wait must be reported, got %d", declined.Error.RetryAfterSeconds)
	}
	if got := len(sender.stickerCalls()); got != 1 {
		tst.Fatalf("a declined send must not dispatch, got %d sends", got)
	}

	// An explicit request is never declined, even inside the window.
	if result := sendStickerID(tst, r, stickerCall(5, 0, false), "sticker_02"); result.Status != "ok" {
		tst.Fatalf("an explicit request must not be declined by the cooldown: %+v", result.Error)
	}

	// The explicit send stamped the chat: a proactive one right behind is declined,
	// and the window is measured from that stamp.
	*clock = clock.Add(25 * time.Second)
	if result := sendStickerID(tst, r, stickerCall(5, 0, true), "sticker_01"); result.Error == nil || result.Error.Code != "sticker_cooldown" {
		tst.Fatalf("a proactive send must not follow right behind an explicit one: %+v", result)
	}
	*clock = clock.Add(5 * time.Second) // exactly the window after the explicit stamp
	if result := sendStickerID(tst, r, stickerCall(5, 0, true), "sticker_01"); result.Status != "ok" {
		tst.Fatalf("after the window a proactive send must pass again: %+v", result.Error)
	}

	// Another chat is independent.
	if result := sendStickerID(tst, r, stickerCall(6, 0, true), "sticker_01"); result.Status != "ok" {
		tst.Fatalf("the cooldown is per chat: %+v", result.Error)
	}
}

func TestOnlyADispatchAttemptAffectsTheCooldown(tst *testing.T) {
	sender := sentOK()
	r, _ := directSticker(tst, sender)
	proactiveNow := func() CallContext { return stickerCall(5, 0, true) }

	// Things that never reach a dispatch leave the chat's slot free.
	sendStickerID(tst, r, proactiveNow(), "no_such_one") // unknown id
	exhausted := proactiveNow()
	exhausted.run.stickerAttempted = true
	sendStickerID(tst, r, exhausted, "sticker_01") // per-turn cap
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _ = r.sendSticker(ctx, proactiveNow(), json.RawMessage(`{"id":"sticker_01"}`)) // pre-dispatch cancel
	_, _ = r.sendSticker(context.Background(), proactiveNow(), json.RawMessage(`{"bad":1}`))
	if r.stickerCooldown.cooling(5, r.stickerClock(), stickerProactiveCooldown) {
		tst.Fatalf("declined, invalid and cancelled calls must not stamp the cooldown")
	}
	if len(sender.stickerCalls()) != 0 {
		tst.Fatalf("none of those may dispatch")
	}
	if result := sendStickerID(tst, r, proactiveNow(), "sticker_01"); result.Status != "ok" {
		tst.Fatalf("a proactive send right after them must still be admitted: %+v", result.Error)
	}

	// A declined admission does not extend the window either.
	r2, clock := directSticker(tst, sentOK())
	sendStickerID(tst, r2, stickerCall(5, 0, true), "sticker_01")
	*clock = clock.Add(20 * time.Second)
	sendStickerID(tst, r2, stickerCall(5, 0, true), "sticker_01") // declined at +20s
	*clock = clock.Add(10 * time.Second)                          // +30s from the real send
	if result := sendStickerID(tst, r2, stickerCall(5, 0, true), "sticker_01"); result.Status != "ok" {
		tst.Fatalf("a declined attempt must not restart the window: %+v", result.Error)
	}

	// An ambiguous outcome keeps the stamp: the message may be out there.
	ambiguous := &stubActionSender{stickerErr: errors.New("lost response")}
	r3, _ := directSticker(tst, ambiguous)
	sendStickerID(tst, r3, stickerCall(5, 0, true), "sticker_01")
	if result := sendStickerID(tst, r3, stickerCall(5, 0, true), "sticker_01"); result.Error == nil || result.Error.Code != "sticker_cooldown" {
		tst.Fatalf("an ambiguous attempt must keep the cooldown stamp: %+v", result)
	}
}

func TestStickerAdmissionIsAtomicAcrossConcurrentProactiveTurns(tst *testing.T) {
	sender := sentOK()
	r, _ := directSticker(tst, sender)

	const turns = 32
	var wg sync.WaitGroup
	var admitted, declined atomic.Int32
	start := make(chan struct{})
	for i := 0; i < turns; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			result := sendStickerID(tst, r, stickerCall(5, 0, true), "sticker_01")
			switch {
			case result.Status == "ok":
				admitted.Add(1)
			case result.Error != nil && result.Error.Code == "sticker_cooldown":
				declined.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()

	if admitted.Load() != 1 || declined.Load() != turns-1 || len(sender.stickerCalls()) != 1 {
		tst.Fatalf("exactly one proactive turn may win the slot: admitted=%d declined=%d sends=%d", admitted.Load(), declined.Load(), len(sender.stickerCalls()))
	}
}

func TestConcurrentExplicitTurnsEachGetTheirOwnSingleAttempt(tst *testing.T) {
	sender := sentOK()
	r, _ := directSticker(tst, sender)

	const turns = 16
	var wg sync.WaitGroup
	var ok atomic.Int32
	for i := 0; i < turns; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			call := stickerCall(5, 0, false)
			if sendStickerID(tst, r, call, "sticker_01").Status == "ok" {
				ok.Add(1)
			}
			if again := sendStickerID(tst, r, call, "sticker_02"); again.Error == nil || again.Error.Code != "sticker_limit_reached" {
				tst.Errorf("a turn's second send must be refused: %+v", again)
			}
		}()
	}
	wg.Wait()

	if ok.Load() != turns || len(sender.stickerCalls()) != turns {
		tst.Fatalf("each explicit turn sends exactly once: ok=%d sends=%d", ok.Load(), len(sender.stickerCalls()))
	}
}

func TestStickerToolsAreHiddenWhenTheyCannotSucceed(tst *testing.T) {
	r, clock := directSticker(tst, sentOK())
	send, _ := r.registry.Lookup(sendStickerToolName)
	list, _ := r.registry.Lookup(listStickersToolName)
	available := func(d Definition, call CallContext) bool {
		_, ok := d.Resolve(call, Presentation{})

		return ok
	}

	if !available(send, stickerCall(5, 0, false)) || !available(list, stickerCall(5, 0, true)) {
		tst.Fatalf("both tools are offered while nothing blocks them")
	}

	spent := stickerCall(5, 0, false)
	spent.run.stickerAttempted = true
	if available(send, spent) {
		tst.Fatalf("send_sticker must be hidden once the turn's attempt is used")
	}
	if !available(list, spent) {
		tst.Fatalf("browsing stays possible after a send")
	}
	lost := stickerCall(5, 0, false)
	lost.run.stickerMisses = stickerMissLimit
	if available(send, lost) {
		tst.Fatalf("send_sticker must be hidden after too many misses")
	}

	sendStickerID(tst, r, stickerCall(5, 0, true), "sticker_01")
	if available(send, stickerCall(5, 0, true)) || available(list, stickerCall(5, 0, true)) {
		tst.Fatalf("proactive turns inside the cooldown get neither tool")
	}
	if !available(send, stickerCall(5, 0, false)) {
		tst.Fatalf("explicit turns are offered the tool regardless of the cooldown")
	}
	if !available(send, stickerCall(6, 0, true)) {
		tst.Fatalf("the cooldown is per chat")
	}
	*clock = clock.Add(stickerProactiveCooldown)
	if !available(send, stickerCall(5, 0, true)) {
		tst.Fatalf("the tool returns after the window")
	}
}

func TestStickerLimiterIsBounded(tst *testing.T) {
	var l chatCooldown
	now := stickerTestNow
	for chat := int64(0); chat < chatCooldownMaxEntries+500; chat++ {
		l.admit(chat, false, now, stickerProactiveCooldown)
	}
	if len(l.last) > chatCooldownMaxEntries {
		tst.Fatalf("the cooldown map must stay bounded, has %d entries", len(l.last))
	}
	if !l.cooling(chatCooldownMaxEntries+499, now, stickerProactiveCooldown) {
		tst.Fatalf("the most recent chat must still be stamped")
	}

	// Expired entries are pruned first when the cap is hit.
	later := now.Add(2 * stickerProactiveCooldown)
	l.admit(1_000_000, false, later, stickerProactiveCooldown)
	if len(l.last) > 2 {
		tst.Fatalf("expired entries must be pruned at the cap, have %d", len(l.last))
	}
}

// Full-runtime flows: the cap and exclusions must hold through the real loop,
// not only in direct handler calls.

func stickerRunRequest(proactive bool) ChatRequest {
	return ChatRequest{
		Scope:          state.ConversationScope{ChatID: 5, TopicID: 9},
		PromptScope:    llm.PromptScope{ChatID: 5},
		ReplyContext:   llm.ChatReplyContext{UserMessage: llm.TextMessage(llm.RoleUser, "send a sticker")},
		RequestMessage: state.Message{FromID: 11, ChatID: 5, MessageID: 42},
		Proactive:      proactive,
	}
}

func sendCall(id, sticker string) llm.ToolCall {
	args, _ := json.Marshal(map[string]string{"id": sticker})

	return llm.ToolCall{ID: id, Name: sendStickerToolName, Arguments: args}
}

func fullStickerRuntime(tst *testing.T, sender *stubActionSender) (*Runtime, *stubLLM) {
	tst.Helper()
	r, m := restrictedRuntime(tst, sender)
	r.AttachStickerCatalog(stickerCatalogForSend(tst))

	return r, m
}

func TestSecondSendInOneModelResponseIsRejectedInEveryOutcome(tst *testing.T) {
	for name, sender := range map[string]*stubActionSender{
		"after success":   sentOK(),
		"after ambiguous": {stickerErr: errors.New("lost response")},
		"after rejection": {stickerErr: &telegoapi.Error{ErrorCode: 400, Description: "Bad Request"}},
	} {
		tst.Run(name, func(tst *testing.T) {
			r, m := fullStickerRuntime(tst, sender)
			m.responses = []llm.Response{
				toolBatch(sendCall("one", "sticker_01"), sendCall("two", "sticker_02")),
				{Message: llm.TextMessage(llm.RoleAssistant, "done")},
			}

			result, err := r.ReplyWithTools(context.Background(), stickerRunRequest(false))
			if err != nil || len(result.ToolCalls) != 2 {
				tst.Fatalf("result=%+v err=%v", result, err)
			}
			first, second := result.ToolCalls[0], result.ToolCalls[1]
			if !first.Executed || !first.SideEffecting || second.Executed || second.Rejection != RejectionUnavailable {
				tst.Fatalf("the second call must be rejected before execution: %+v / %+v", first, second)
			}
			if got := len(sender.stickerCalls()); got != 1 {
				tst.Fatalf("the runtime must enforce one dispatch attempt per turn, got %d", got)
			}
			wantOutcome := map[string]string{"after success": "confirmed", "after ambiguous": "unknown", "after rejection": "not_applied"}[name]
			if first.SideEffectOutcome != wantOutcome {
				tst.Fatalf("side-effect outcome %q, want %q", first.SideEffectOutcome, wantOutcome)
			}
		})
	}
}

func TestStickerIsACompleteReplyAndAllowsSayNothing(tst *testing.T) {
	sender := sentOK()
	r, m := fullStickerRuntime(tst, sender)
	m.responses = []llm.Response{
		toolBatch(sendCall("one", "sticker_01"), llm.ToolCall{ID: "quiet", Name: "say_nothing", Arguments: json.RawMessage(`{}`)}),
	}

	result, err := r.ReplyWithTools(context.Background(), stickerRunRequest(false))
	if err != nil || !result.IntentionalSilence || len(sender.stickerCalls()) != 1 {
		tst.Fatalf("a sticker followed by say_nothing is a complete reply: %+v %v", result, err)
	}
	if result.ToolCalls[0].SideEffectOutcome != "confirmed" || !result.ToolCalls[0].SideEffecting {
		tst.Fatalf("the sticker send must be recorded as a confirmed side effect: %+v", result.ToolCalls[0])
	}

	// A confirmed sticker also lets the model finish with no text at all.
	sender2 := sentOK()
	r2, m2 := fullStickerRuntime(tst, sender2)
	m2.responses = []llm.Response{toolBatch(sendCall("one", "sticker_01")), {Message: llm.TextMessage(llm.RoleAssistant, "")}}
	if _, err := r2.ReplyWithTools(context.Background(), stickerRunRequest(false)); err != nil {
		tst.Fatalf("an empty reply after a confirmed sticker must be accepted: %v", err)
	}
}

func TestRestrictedReactionOnlyTurnsCannotSendStickers(tst *testing.T) {
	sender := sentOK()
	r, m := fullStickerRuntime(tst, sender)
	m.responses = []llm.Response{
		toolBatch(sendCall("sneaky", "sticker_01")),
		{Message: llm.TextMessage(llm.RoleAssistant, "done")},
	}

	req := restrictedReactionRequest()
	req.CompleteAfterSideEffect = false
	result, err := r.ReplyWithTools(context.Background(), req)
	if err != nil {
		tst.Fatalf("restricted run: %v", err)
	}
	if len(sender.stickerCalls()) != 0 || len(result.ToolCalls) != 1 || result.ToolCalls[0].Executed {
		tst.Fatalf("a reaction-only turn must not send stickers: %+v", result.ToolCalls)
	}
	for _, round := range m.buildTools {
		for _, def := range round {
			if def.Name == sendStickerToolName || def.Name == listStickersToolName {
				tst.Fatalf("%s must not be advertised to a reaction-only turn", def.Name)
			}
		}
	}
}

func TestProactiveFlagComesFromTheRequestNotTheModel(tst *testing.T) {
	sender := sentOK()
	r, m := fullStickerRuntime(tst, sender)
	clock := stickerTestNow
	r.stickerNow = func() time.Time { return clock }

	// A proactive run sends once; the next proactive run in the chat inside the
	// window no longer even sees the tool.
	m.responses = []llm.Response{toolBatch(sendCall("one", "sticker_01")), {Message: llm.TextMessage(llm.RoleAssistant, "done")}}
	if _, err := r.ReplyWithTools(context.Background(), stickerRunRequest(true)); err != nil {
		tst.Fatal(err)
	}
	m.responses = []llm.Response{{Message: llm.TextMessage(llm.RoleAssistant, "done")}}
	if _, err := r.ReplyWithTools(context.Background(), stickerRunRequest(true)); err != nil {
		tst.Fatal(err)
	}
	for _, def := range m.lastBuildTools {
		if def.Name == sendStickerToolName || def.Name == listStickersToolName {
			tst.Fatalf("%s must be hidden from a proactive turn inside the cooldown", def.Name)
		}
	}

	// An explicit turn in the same chat is offered the tool and can send.
	m.responses = []llm.Response{toolBatch(sendCall("two", "sticker_02")), {Message: llm.TextMessage(llm.RoleAssistant, "done")}}
	result, err := r.ReplyWithTools(context.Background(), stickerRunRequest(false))
	if err != nil || result.ToolCalls[0].SideEffectOutcome != "confirmed" {
		tst.Fatalf("an explicit request must go through the cooldown: %+v %v", result.ToolCalls, err)
	}
	if got := len(sender.stickerCalls()); got != 2 {
		tst.Fatalf("expected two sends in total, got %d", got)
	}
}

func TestStickerToolsAbsentWithoutACatalog(tst *testing.T) {
	r, m := restrictedRuntime(tst, sentOK())
	m.responses = []llm.Response{{Message: llm.TextMessage(llm.RoleAssistant, "done")}}
	if _, err := r.ReplyWithTools(context.Background(), stickerRunRequest(false)); err != nil {
		tst.Fatal(err)
	}
	for _, def := range m.lastBuildTools {
		if def.Name == sendStickerToolName || def.Name == listStickersToolName {
			tst.Fatalf("%s must not exist without an attached catalog", def.Name)
		}
	}
}
