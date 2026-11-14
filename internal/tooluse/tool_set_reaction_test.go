package tooluse

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"telegram-ollama-reply-bot/internal/execution"
	"telegram-ollama-reply-bot/internal/llm"
	"telegram-ollama-reply-bot/internal/state"
	"telegram-ollama-reply-bot/internal/state/memory"
	"telegram-ollama-reply-bot/internal/telegram/reactionemoji"

	tg "github.com/mymmrac/telego"
	telegoapi "github.com/mymmrac/telego/telegoapi"
	tu "github.com/mymmrac/telego/telegoutil"
)

// --- helpers ---

var testBotIdentity = BotIdentity{UserID: 999, Name: "MyBot", Username: "mybot"}

// newSetReactionRuntime builds a runtime with the reaction coordination
// timings shrunk for fast tests. Pass nil store for no history.
func newSetReactionRuntime(t *testing.T, store state.ConversationStore, sender *stubActionSender) *Runtime {
	t.Helper()

	if store == nil {
		store = memory.New(memory.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil))).Conversations()
	}

	runtime := New(
		&stubLLM{},
		store,
		&stubExtractor{},
		nil,
		sender,
		nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		Config{MaxIterations: 6, BotIdentity: testBotIdentity},
	)
	runtime.reactions.spacing = 10 * time.Millisecond
	runtime.reactions.pad = 10 * time.Millisecond
	runtime.reactions.maxWait = 300 * time.Millisecond

	return runtime
}

func reactionCallCtx(scope state.ConversationScope, requester state.Message) CallContext {
	return CallContext{
		Scope:     scope,
		Requester: requester,
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		RunID:     "run-test",
		run:       &runState{},
	}
}

func callSetReaction(t *testing.T, runtime *Runtime, callCtx CallContext, args string) toolResult {
	t.Helper()

	result, err := runtime.setReaction(context.Background(), callCtx, json.RawMessage(args))
	if err != nil {
		t.Fatalf("setReaction returned a Go error: %v", err)
	}

	return result
}

// reactionErrorCode extracts the structured error code from a tool result.
func reactionErrorCode(t *testing.T, result toolResult) string {
	t.Helper()

	if result.Status != execution.StatusError {
		t.Fatalf("expected error result, got status %q (%+v)", result.Status, result.Data)
	}
	if result.Error != "" {
		t.Fatalf("structured tool errors must not carry a raw Go error string, got %q", result.Error)
	}

	data, ok := result.Data.(map[string]any)
	if !ok {
		t.Fatalf("expected structured error data, got %#v", result.Data)
	}
	errObj, ok := data["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected error object, got %#v", data)
	}
	code, _ := errObj["code"].(string)

	return code
}

func reactionUserMessage(scope state.ConversationScope) (state.ConversationScope, state.Message) {
	return scope, state.Message{FromID: 11, ChatID: scope.ChatID, TopicID: scope.TopicID, MessageID: 42, Name: "alice"}
}

// --- definition, schema, guidance ---

func TestSetReactionDefinitionPolicy(t *testing.T) {
	t.Parallel()

	runtime := newSetReactionRuntime(t, nil, &stubActionSender{})
	definition, ok := runtime.registry.Lookup("set_reaction")
	if !ok {
		t.Fatal("set_reaction must be registered by default")
	}
	if definition.InvocationPolicy != InvocationPolicyDiscretionary {
		t.Fatalf("unexpected invocation policy: %q", definition.InvocationPolicy)
	}
	if !definition.SideEffecting {
		t.Fatal("set_reaction must be side-effecting")
	}
	if definition.EffectiveVisibility() != VisibilityStandard {
		t.Fatalf("unexpected visibility: %q", definition.Visibility)
	}
	if definition.ResultCharBudget != setReactionResultCharBudget {
		t.Fatalf("unexpected result char budget: %d", definition.ResultCharBudget)
	}
}

func TestSetReactionDescriptionAndEnumMatchWhitelist(t *testing.T) {
	t.Parallel()

	runtime := newSetReactionRuntime(t, nil, &stubActionSender{})
	definition, ok := runtime.registry.Lookup("set_reaction")
	if !ok {
		t.Fatal("set_reaction must be registered by default")
	}

	// The description enumerates exactly the shared whitelist, in order.
	whitelist := reactionemoji.All()
	joined := reactionemoji.Joined()
	if !strings.Contains(definition.Description, "Available emoji: "+joined) {
		t.Fatalf("description must list the shared whitelist verbatim:\n%s", definition.Description)
	}

	// The schema enum is generated from the same source and in the same
	// order; the only argument is the required emoji.
	var schema struct {
		Type       string `json:"type"`
		Properties struct {
			Emoji struct {
				Type string   `json:"type"`
				Enum []string `json:"enum"`
			} `json:"emoji"`
		} `json:"properties"`
		Required          []string `json:"required"`
		AdditionalProps   bool     `json:"additionalProperties"`
		MessageIDProperty bool     `json:"-"`
		ChatIDProperty    bool     `json:"-"`
	}
	if err := json.Unmarshal(definition.Parameters, &schema); err != nil {
		t.Fatalf("schema must be valid JSON: %v", err)
	}

	raw := string(definition.Parameters)
	if strings.Contains(raw, "message_id") || strings.Contains(raw, "chat_id") {
		t.Fatal("schema must not expose message or chat identifiers")
	}
	if !strings.Contains(raw, `"additionalProperties":false`) {
		t.Fatal("schema must reject additional properties")
	}
	if len(schema.Required) != 1 || schema.Required[0] != "emoji" {
		t.Fatalf("only emoji may be required, got %v", schema.Required)
	}

	if len(schema.Properties.Emoji.Enum) != len(whitelist) {
		t.Fatalf("enum size %d must match whitelist size %d", len(schema.Properties.Emoji.Enum), len(whitelist))
	}
	for i, emoji := range whitelist {
		if schema.Properties.Emoji.Enum[i] != emoji {
			t.Fatalf("enum[%d] = %q, want %q (order must be deterministic)", i, schema.Properties.Emoji.Enum[i], emoji)
		}
	}
}

func TestSetReactionHeartEmojiExactForm(t *testing.T) {
	t.Parallel()

	sender := &stubActionSender{}
	runtime := newSetReactionRuntime(t, nil, sender)
	scope, requester := reactionUserMessage(state.ConversationScope{ChatID: 1})

	result := callSetReaction(t, runtime, reactionCallCtx(scope, requester), `{"emoji":"❤"}`)
	if result.Status != execution.StatusOK {
		t.Fatalf("bare heart U+2764 must be accepted, got %+v", result.Data)
	}
	if sender.lastReactionParams == nil || len(sender.lastReactionParams.Reaction) != 1 {
		t.Fatalf("expected one reaction type sent, got %+v", sender.lastReactionParams)
	}
	sent, ok := sender.lastReactionParams.Reaction[0].(*tg.ReactionTypeEmoji)
	if !ok || sent.Emoji != "❤" {
		t.Fatalf("exact heart bytes must be forwarded, got %#v", sender.lastReactionParams.Reaction[0])
	}

	// The variation-selector form must be rejected before any API call —
	// and the rejection must not consume a fresh run's attempt.
	fresh := reactionCallCtx(scope, requester)
	if code := reactionErrorCode(t, callSetReaction(t, runtime, fresh, `{"emoji":"❤️"}`)); code != "invalid_emoji" {
		t.Fatalf("heart with variation selector must be invalid_emoji, got %q", code)
	}
	if result := callSetReaction(t, runtime, fresh, `{"emoji":"👍"}`); result.Status != execution.StatusOK {
		t.Fatalf("invalid attempt must not consume the run attempt, got %+v", result.Data)
	}
}

// --- local validation before Telegram ---

func TestSetReactionRejectsInvalidArgumentsBeforeTelegram(t *testing.T) {
	t.Parallel()

	sender := &stubActionSender{}
	runtime := newSetReactionRuntime(t, nil, sender)
	scope, requester := reactionUserMessage(state.ConversationScope{ChatID: 1})

	for name, args := range map[string]string{
		"not json":       `emoji`,
		"unknown field":  `{"emoji":"👍","message_id":7}`,
		"missing emoji":  `{}`,
		"empty emoji":    `{"emoji":""}`,
		"unsupported":    `{"emoji":"😁"}`,
		"multiple emoji": `{"emoji":"👍👎"}`,
	} {
		result := callSetReaction(t, runtime, reactionCallCtx(scope, requester), args)
		code := reactionErrorCode(t, result)
		if code != "invalid_arguments" && code != "invalid_emoji" {
			t.Fatalf("%s: unexpected code %q", name, code)
		}
	}

	if sender.reactionCallCount() != 0 {
		t.Fatalf("invalid arguments must never reach Telegram, got %d calls", sender.reactionCallCount())
	}

	// None of the invalid attempts consumed the run's single reaction.
	result := callSetReaction(t, runtime, reactionCallCtx(scope, requester), `{"emoji":"👍"}`)
	if result.Status != execution.StatusOK {
		t.Fatalf("valid call after invalid ones must succeed, got %+v", result.Data)
	}
}

func TestSetReactionRejectsBadTargets(t *testing.T) {
	t.Parallel()

	sender := &stubActionSender{}
	runtime := newSetReactionRuntime(t, nil, sender)

	cases := map[string]CallContext{
		"no chat": {
			Scope:     state.ConversationScope{},
			Requester: state.Message{MessageID: 42},
			Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
			RunID:     "run", run: &runState{},
		},
		"no message id": {
			Scope:     state.ConversationScope{ChatID: 1},
			Requester: state.Message{FromID: 11},
			Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
			RunID:     "run", run: &runState{},
		},
		"negative message id": {
			Scope:     state.ConversationScope{ChatID: 1},
			Requester: state.Message{MessageID: -5},
			Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
			RunID:     "run", run: &runState{},
		},
		"bot-authored target": {
			Scope:     state.ConversationScope{ChatID: 1},
			Requester: state.Message{IsMe: true, MessageID: 42},
			Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
			RunID:     "run", run: &runState{},
		},
		"chat mismatch": {
			Scope:     state.ConversationScope{ChatID: 2},
			Requester: state.Message{ChatID: 1, MessageID: 42},
			Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
			RunID:     "run", run: &runState{},
		},
		"topic mismatch": {
			Scope:     state.ConversationScope{ChatID: 1, TopicID: 5},
			Requester: state.Message{ChatID: 1, TopicID: 7, MessageID: 42},
			Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
			RunID:     "run", run: &runState{},
		},
		"media group": {
			Scope:                 state.ConversationScope{ChatID: 1},
			Requester:             state.Message{ChatID: 1, MessageID: 42},
			RequesterMediaGroupID: "album-1",
			Logger:                slog.New(slog.NewTextHandler(io.Discard, nil)),
			RunID:                 "run", run: &runState{},
		},
	}

	for name, callCtx := range cases {
		result := callSetReaction(t, runtime, callCtx, `{"emoji":"👍"}`)
		code := reactionErrorCode(t, result)
		if code != "invalid_target" && code != "media_group_target" {
			t.Fatalf("%s: unexpected code %q", name, code)
		}
	}

	if sender.reactionCallCount() != 0 {
		t.Fatalf("rejected targets must never reach Telegram, got %d calls", sender.reactionCallCount())
	}
}

func TestSetReactionRequiresRunStateAndIdentity(t *testing.T) {
	t.Parallel()

	sender := &stubActionSender{}
	runtime := newSetReactionRuntime(t, nil, sender)
	scope, requester := reactionUserMessage(state.ConversationScope{ChatID: 1})

	withoutRun := reactionCallCtx(scope, requester)
	withoutRun.run = nil
	if code := reactionErrorCode(t, callSetReaction(t, runtime, withoutRun, `{"emoji":"👍"}`)); code != "run_state_unavailable" {
		t.Fatalf("missing run state must be refused, got %q", code)
	}

	identityless := New(
		&stubLLM{},
		nil,
		&stubExtractor{},
		nil,
		sender,
		nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		Config{},
	)
	if code := reactionErrorCode(t, callSetReaction(t, identityless, reactionCallCtx(scope, requester), `{"emoji":"👍"}`)); code != "identity_unavailable" {
		t.Fatalf("missing identity must be refused, got %q", code)
	}

	if sender.reactionCallCount() != 0 {
		t.Fatalf("refused calls must never reach Telegram, got %d calls", sender.reactionCallCount())
	}
}

// --- run attempt bound ---

func TestSetReactionOneAttemptPerRun(t *testing.T) {
	t.Parallel()

	sender := &stubActionSender{}
	runtime := newSetReactionRuntime(t, nil, sender)
	scope, requester := reactionUserMessage(state.ConversationScope{ChatID: 1})
	callCtx := reactionCallCtx(scope, requester)

	if result := callSetReaction(t, runtime, callCtx, `{"emoji":"👍"}`); result.Status != execution.StatusOK {
		t.Fatalf("first reaction must succeed, got %+v", result.Data)
	}
	if code := reactionErrorCode(t, callSetReaction(t, runtime, callCtx, `{"emoji":"🔥"}`)); code != "already_reacted" {
		t.Fatalf("second attempt in one run must be refused, got %q", code)
	}
	if got := sender.reactionCallCount(); got != 1 {
		t.Fatalf("exactly one outgoing API attempt is allowed per run, got %d", got)
	}

	// A separate run has its own attempt budget.
	other := reactionCallCtx(scope, requester)
	other.run = &runState{}
	other.RunID = "run-other"
	if result := callSetReaction(t, runtime, other, `{"emoji":"🔥"}`); result.Status != execution.StatusOK {
		t.Fatalf("a new run must get a fresh attempt, got %+v", result.Data)
	}
	if got := sender.reactionCallCount(); got != 2 {
		t.Fatalf("unexpected total API calls: %d", got)
	}
}

// --- success path: state, event, evicted target, dedup ---

func TestSetReactionSuccessWritesHistoryAndPublishesEvent(t *testing.T) {
	t.Parallel()

	store := memory.New(memory.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil))).Conversations()
	scope := state.ConversationScope{ChatID: 100, TopicID: 7}
	store.AppendMessage(scope, state.Message{Text: "a joke", FromID: 11, MessageID: 42, Name: "alice"})

	sender := &stubActionSender{}
	runtime := newSetReactionRuntime(t, store, sender)
	recorder := &recordingDispatcher{}
	runtime.config.Dispatcher = recorder

	result := callSetReaction(t, runtime, reactionCallCtx(scope, state.Message{FromID: 11, ChatID: 100, TopicID: 7, MessageID: 42}), `{"emoji":"😂"}`)
	if result.Status != execution.StatusOK {
		t.Fatalf("expected success, got %+v", result.Data)
	}

	// Exactly the resolved run-bound target with one ordinary emoji.
	params := sender.lastReactionParams
	if params == nil {
		t.Fatal("expected a SetMessageReaction call")
	}
	if params.ChatID.ID != 100 || params.MessageID != 42 {
		t.Fatalf("unexpected target: %+v", params)
	}
	if params.IsBig {
		t.Fatal("is_big must stay false")
	}
	if len(params.Reaction) != 1 {
		t.Fatalf("exactly one reaction type expected, got %d", len(params.Reaction))
	}
	sent, ok := params.Reaction[0].(*tg.ReactionTypeEmoji)
	if !ok || sent.Emoji != "😂" || sent.Type != tg.ReactionEmoji {
		t.Fatalf("unexpected reaction type: %#v", params.Reaction[0])
	}

	// The authoritative local record sits on the triggering user message
	// with the real bot identity.
	snapshot := store.Snapshot(scope)
	if len(snapshot.Messages) != 1 {
		t.Fatalf("no message may be fabricated, got %d", len(snapshot.Messages))
	}
	reactions := snapshot.Messages[0].Reactions
	if len(reactions) != 1 {
		t.Fatalf("expected one stored reaction, got %#v", reactions)
	}
	if reactions[0].UserID != testBotIdentity.UserID || reactions[0].Name != testBotIdentity.Name || reactions[0].Username != testBotIdentity.Username || reactions[0].Emoji != "😂" {
		t.Fatalf("unexpected bot attribution: %#v", reactions[0])
	}

	// Exactly one run-correlated observational event, after the history
	// write, carrying no fabricated incoming-update data.
	botEvents := eventsOf[execution.BotReactionSet](recorder.events)
	if len(botEvents) != 1 {
		t.Fatalf("expected exactly one BotReactionSet, got %d", len(botEvents))
	}
	event := botEvents[0]
	if event.RunID != "run-test" || event.ChatID != 100 || event.TopicID != 7 || event.MessageID != 42 {
		t.Fatalf("unexpected run context: %+v", event.RunContext)
	}
	if event.TargetMessageID != 42 || event.BotUserID != testBotIdentity.UserID || event.Emoji != "😂" || event.At.IsZero() {
		t.Fatalf("unexpected event payload: %+v", event)
	}

	// The model-visible result carries the emoji only: no identifiers.
	data, ok := result.Data.(map[string]any)
	if !ok || len(data) != 1 || data["emoji"] != "😂" {
		t.Fatalf("result data must be the emoji only, got %#v", result.Data)
	}
}

func TestSetReactionEvictedTargetStillSucceeds(t *testing.T) {
	t.Parallel()

	// No history: the target was never stored (or evicted after a restart,
	// trim, or reset). The Telegram send still counts as a successful tool
	// action; nothing is resurrected.
	store := memory.New(memory.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil))).Conversations()
	sender := &stubActionSender{}
	runtime := newSetReactionRuntime(t, store, sender)
	recorder := &recordingDispatcher{}
	runtime.config.Dispatcher = recorder

	scope := state.ConversationScope{ChatID: 200}
	result := callSetReaction(t, runtime, reactionCallCtx(scope, state.Message{FromID: 11, ChatID: 200, MessageID: 42}), `{"emoji":"🙏"}`)
	if result.Status != execution.StatusOK {
		t.Fatalf("confirmed send must succeed despite missing history, got %+v", result.Data)
	}
	if len(store.Snapshot(scope).Messages) != 0 {
		t.Fatal("no message may be invented for a vanished target")
	}
	if got := eventsOf[execution.BotReactionSet](recorder.events); len(got) != 1 {
		t.Fatalf("expected exactly one event despite eviction, got %d", len(got))
	}
}

func TestSetReactionNoDuplicateLocalEntry(t *testing.T) {
	t.Parallel()

	store := memory.New(memory.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil))).Conversations()
	scope := state.ConversationScope{ChatID: 300}
	store.AppendMessage(scope, state.Message{Text: "thanks", FromID: 11, MessageID: 42})

	sender := &stubActionSender{}
	runtime := newSetReactionRuntime(t, store, sender)

	first := callSetReaction(t, runtime, reactionCallCtx(scope, state.Message{FromID: 11, ChatID: 300, MessageID: 42}), `{"emoji":"🙏"}`)
	if first.Status != execution.StatusOK {
		t.Fatalf("first reaction failed: %+v", first.Data)
	}
	before := store.Snapshot(scope)

	// A second run repeats the identical reaction: Telegram is called again
	// (each confirmed send counts), but the local record must not duplicate.
	secondCtx := reactionCallCtx(scope, state.Message{FromID: 11, ChatID: 300, MessageID: 42})
	secondCtx.run = &runState{}
	secondCtx.RunID = "run-2"
	if result := callSetReaction(t, runtime, secondCtx, `{"emoji":"🙏"}`); result.Status != execution.StatusOK {
		t.Fatalf("second reaction failed: %+v", result.Data)
	}

	after := store.Snapshot(scope)
	if len(after.Messages[0].Reactions) != 1 {
		t.Fatalf("identical reaction must not duplicate the entry: %#v", after.Messages[0].Reactions)
	}
	if after.Revision != before.Revision || after.ApproxBytes != before.ApproxBytes {
		t.Fatal("identical reaction must not churn accounting")
	}
	if sender.reactionCallCount() != 2 {
		t.Fatalf("each run sends its own attempt, got %d", sender.reactionCallCount())
	}
}

// --- Telegram failure handling ---

func TestSetReactionTelegramAPIError(t *testing.T) {
	t.Parallel()

	store := memory.New(memory.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil))).Conversations()
	scope := state.ConversationScope{ChatID: 400}
	store.AppendMessage(scope, state.Message{Text: "question", FromID: 11, MessageID: 42})
	baseRevision := store.Snapshot(scope).Revision

	// Telegram-rejected reaction (e.g. not allowed in this chat).
	sender := &stubActionSender{reactionErr: &telegoapi.Error{ErrorCode: 400, Description: "Bad Request: REACTION_INVALID"}}
	runtime := newSetReactionRuntime(t, store, sender)
	recorder := &recordingDispatcher{}
	runtime.config.Dispatcher = recorder

	result := callSetReaction(t, runtime, reactionCallCtx(scope, state.Message{FromID: 11, ChatID: 400, MessageID: 42}), `{"emoji":"👎"}`)
	if code := reactionErrorCode(t, result); code != "telegram_error" {
		t.Fatalf("expected telegram_error, got %q", code)
	}
	if len(store.Snapshot(scope).Messages[0].Reactions) != 0 {
		t.Fatal("failed reaction must not write state")
	}
	if got := eventsOf[execution.BotReactionSet](recorder.events); len(got) != 0 {
		t.Fatalf("failed reaction must not publish events, got %d", len(got))
	}
	if store.Snapshot(scope).Revision != baseRevision {
		t.Fatal("failed reaction must not bump the history revision")
	}

	// No fallback emoji: the failure is reported as-is.
	if sender.reactionCallCount() != 1 {
		t.Fatalf("no automatic emoji substitution is allowed, got %d calls", sender.reactionCallCount())
	}
}

func TestSetReactionTransportErrorIsSanitized(t *testing.T) {
	t.Parallel()

	// Transport failures can embed the token-bearing request URL. Neither
	// the model-facing result nor any log output may carry it, and an
	// ambiguous failure must not be retried automatically.
	//nolint:gosec // fake token literal for a sanitation assertion
	tokenURL := `Post "https://api.telegram.org/bot1:SECRETTOKEN/setMessageReaction": connection reset`
	sender := &stubActionSender{reactionErr: &urlErrorMock{url: tokenURL, text: tokenURL}}
	runtime := newSetReactionRuntime(t, nil, sender)

	scope := state.ConversationScope{ChatID: 500}
	var logBuf strings.Builder
	callCtx := reactionCallCtx(scope, state.Message{FromID: 11, ChatID: 500, MessageID: 42})
	callCtx.Logger = slog.New(slog.NewTextHandler(&logBuf, nil))

	result, err := runtime.setReaction(context.Background(), callCtx, json.RawMessage(`{"emoji":"👍"}`))
	if err != nil {
		t.Fatalf("setReaction returned a Go error: %v", err)
	}
	if code := reactionErrorCode(t, result); code != "request_failed" {
		t.Fatalf("expected request_failed, got %q", code)
	}

	wire, marshalErr := json.Marshal(result)
	if marshalErr != nil {
		t.Fatalf("marshal result: %v", marshalErr)
	}
	if strings.Contains(string(wire), "SECRETTOKEN") || strings.Contains(string(wire), "api.telegram.org") {
		t.Fatalf("model-facing result must be sanitized, got %s", wire)
	}
	if strings.Contains(logBuf.String(), "SECRETTOKEN") || strings.Contains(logBuf.String(), "api.telegram.org") {
		t.Fatalf("logs must be sanitized, got %s", logBuf.String())
	}
	if sender.reactionCallCount() != 1 {
		t.Fatalf("ambiguous transport failures must not be retried, got %d calls", sender.reactionCallCount())
	}
}

// urlErrorMock mimics a raw transport error carrying a token-bearing URL.
type urlErrorMock struct {
	url  string
	text string
}

func (e *urlErrorMock) Error() string { return e.text }

func (e *urlErrorMock) URL() string { return e.url }

func TestSetReactionRateLimitedAbsorbsRetryAfter(t *testing.T) {
	t.Parallel()

	sender := &stubActionSender{}
	runtime := newSetReactionRuntime(t, nil, sender)
	scope := state.ConversationScope{ChatID: 600}

	// Telegram reports a retry window far beyond the per-call wait bound.
	sender.reactionErr = &telegoapi.Error{
		ErrorCode:   429,
		Description: "Too Many Requests: retry after 3600",
		Parameters:  &telegoapi.ResponseParameters{RetryAfter: 3600},
	}
	result := callSetReaction(t, runtime, reactionCallCtx(scope, state.Message{FromID: 11, ChatID: 600, MessageID: 42}), `{"emoji":"👍"}`)
	if code := reactionErrorCode(t, result); code != "rate_limited" {
		t.Fatalf("expected rate_limited, got %q", code)
	}
	if sender.reactionCallCount() != 1 {
		t.Fatalf("unexpected call count: %d", sender.reactionCallCount())
	}

	// The full Telegram deadline must stand (uncapped): a later run in the
	// same chat refuses fast instead of waiting or sending into the window.
	sender.reactionErr = nil
	start := time.Now()
	other := reactionCallCtx(scope, state.Message{FromID: 11, ChatID: 600, MessageID: 43})
	other.run = &runState{}
	result = callSetReaction(t, runtime, other, `{"emoji":"🔥"}`)
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Fatalf("refusal must be immediate, took %v", elapsed)
	}
	if code := reactionErrorCode(t, result); code != "rate_limited" {
		t.Fatalf("expected refusal within the retry window, got %q", code)
	}
	if sender.reactionCallCount() != 1 {
		t.Fatal("no send may happen inside the rate-limit window")
	}

	// Other chats are unaffected.
	otherChat := reactionCallCtx(state.ConversationScope{ChatID: 601}, state.Message{FromID: 11, ChatID: 601, MessageID: 42})
	if result := callSetReaction(t, runtime, otherChat, `{"emoji":"👍"}`); result.Status != execution.StatusOK {
		t.Fatalf("cooldowns must be per chat, got %+v", result.Data)
	}
}

func TestSetReactionCancelledWhileWaitingCooldown(t *testing.T) {
	t.Parallel()

	sender := &stubActionSender{}
	runtime := newSetReactionRuntime(t, nil, sender)
	scope := state.ConversationScope{ChatID: 700}
	callCtx := reactionCallCtx(scope, state.Message{FromID: 11, ChatID: 700, MessageID: 42})

	// Put the chat into a cooldown long enough to require a wait, but
	// within the per-call wait bound so the call blocks until cancelled.
	runtime.reactions.mu.Lock()
	runtime.reactions.cooldowns = map[int64]time.Time{700: time.Now().Add(100 * time.Millisecond)}
	runtime.reactions.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	result, err := runtime.setReaction(ctx, callCtx, json.RawMessage(`{"emoji":"👍"}`))
	if err != nil {
		t.Fatalf("setReaction returned a Go error: %v", err)
	}
	if code := reactionErrorCode(t, result); code != "cancelled" {
		t.Fatalf("expected cancelled, got %q", code)
	}
	if sender.reactionCallCount() != 0 {
		t.Fatal("a cancelled wait must not send")
	}

	// The cancellation consumed nothing: the same run's next call proceeds
	// once the chat is free again.
	runtime.reactions.mu.Lock()
	delete(runtime.reactions.cooldowns, 700)
	runtime.reactions.mu.Unlock()
	result = callSetReaction(t, runtime, callCtx, `{"emoji":"👍"}`)
	if result.Status != execution.StatusOK {
		t.Fatalf("cancelled wait must not consume the run attempt, got %+v", result.Data)
	}
	if sender.reactionCallCount() != 1 {
		t.Fatalf("unexpected call count: %d", sender.reactionCallCount())
	}
}

// --- cross-run coordination ---

func TestSetReactionSerializesSameTarget(t *testing.T) {
	t.Parallel()

	sender := &stubActionSender{reactionDelay: 60 * time.Millisecond}
	runtime := newSetReactionRuntime(t, nil, sender)
	scope := state.ConversationScope{ChatID: 800}

	const runs = 3
	var wg sync.WaitGroup
	for i := 0; i < runs; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()

			callCtx := reactionCallCtx(scope, state.Message{FromID: 11, ChatID: 800, MessageID: 42})
			result, err := runtime.setReaction(context.Background(), callCtx, json.RawMessage(`{"emoji":"👍"}`))
			if err != nil || result.Status != execution.StatusOK {
				t.Errorf("concurrent reaction failed: %v %+v", err, result.Data)
			}
		}()
	}
	wg.Wait()

	// Actual API call starts must be strictly ordered: no two sends to the
	// same target may overlap.
	starts := sender.reactionStarts()
	if len(starts) != runs {
		t.Fatalf("expected %d sends, got %d", runs, len(starts))
	}
	for i := 1; i < len(starts); i++ {
		if !starts[i].After(starts[i-1]) {
			t.Fatalf("same-target sends overlapped: call %d at %v not after call %d at %v", i, starts[i], i-1, starts[i-1])
		}
	}
}

func TestSetReactionDoesNotSerializeDifferentChats(t *testing.T) {
	t.Parallel()

	sender := &stubActionSender{reactionDelay: 60 * time.Millisecond}
	runtime := newSetReactionRuntime(t, nil, sender)

	var wg sync.WaitGroup
	var firstEnd time.Time
	wg.Add(2)
	go func() {
		defer wg.Done()

		callCtx := reactionCallCtx(state.ConversationScope{ChatID: 900}, state.Message{FromID: 11, ChatID: 900, MessageID: 42})
		result, err := runtime.setReaction(context.Background(), callCtx, json.RawMessage(`{"emoji":"👍"}`))
		if err != nil || result.Status != execution.StatusOK {
			t.Errorf("reaction failed: %v %+v", err, result.Data)
		}
		firstEnd = time.Now()
	}()
	go func() {
		defer wg.Done()

		time.Sleep(10 * time.Millisecond) // let the first send begin
		callCtx := reactionCallCtx(state.ConversationScope{ChatID: 901}, state.Message{FromID: 11, ChatID: 901, MessageID: 42})
		result, err := runtime.setReaction(context.Background(), callCtx, json.RawMessage(`{"emoji":"👍"}`))
		if err != nil || result.Status != execution.StatusOK {
			t.Errorf("reaction failed: %v %+v", err, result.Data)
		}
	}()
	wg.Wait()

	// The second chat's actual API call must have STARTED before the first
	// chat's send finished: cross-chat sends may overlap.
	starts := sender.reactionStarts()
	if len(starts) != 2 {
		t.Fatalf("expected two sends, got %d", len(starts))
	}
	if !starts[1].Before(firstEnd) {
		t.Fatalf("different chats must not serialize: second send started %v, first ended %v", starts[1], firstEnd)
	}
}

func TestSetReactionChatSpacingAcrossTargets(t *testing.T) {
	t.Parallel()

	// Two runs, same chat, different targets: the actual API calls respect
	// the chat spacing even though each waited on its own target gate.
	sender := &stubActionSender{}
	runtime := newSetReactionRuntime(t, nil, sender)
	scope := state.ConversationScope{ChatID: 1000}

	first := callSetReaction(t, runtime, reactionCallCtx(scope, state.Message{FromID: 11, ChatID: 1000, MessageID: 42}), `{"emoji":"👍"}`)
	if first.Status != execution.StatusOK {
		t.Fatalf("first reaction failed: %+v", first.Data)
	}

	other := reactionCallCtx(scope, state.Message{FromID: 12, ChatID: 1000, MessageID: 43})
	other.run = &runState{}
	second := callSetReaction(t, runtime, other, `{"emoji":"🔥"}`)
	if second.Status != execution.StatusOK {
		t.Fatalf("second reaction failed: %+v", second.Data)
	}

	starts := sender.reactionStarts()
	if len(starts) != 2 {
		t.Fatalf("expected two sends, got %d", len(starts))
	}
	spacing := runtime.reactions.spacingDuration()
	if gap := starts[1].Sub(starts[0]); gap < spacing {
		t.Fatalf("same-chat sends must be spaced by %v, got %v", spacing, gap)
	}
}

// --- limiter internals ---

func TestReactionLimiterNeverDropsActiveEntries(t *testing.T) {
	t.Parallel()

	var limiter reactionLimiter
	limiter.spacing = time.Millisecond
	limiter.pad = time.Millisecond
	limiter.maxWait = 50 * time.Millisecond

	// Fill the gate map beyond the soft cap with held and waited-on gates.
	ctx, cancel := context.WithCancel(context.Background())
	held := make([]*reactionGate, 0, reactionGateMaxEntries+20)
	for i := 0; i < reactionGateMaxEntries+20; i++ {
		gate, err := limiter.acquire(ctx, int64(i), 1)
		if err != nil {
			t.Fatalf("acquire: %v", err)
		}
		held = append(held, gate)
	}

	limiter.mu.Lock()
	if len(limiter.gates) < reactionGateMaxEntries+20 {
		t.Fatalf("active gates must not be dropped, map holds %d", len(limiter.gates))
	}
	limiter.mu.Unlock()

	// Every held gate still serializes: a second acquire on the same target
	// blocks until release.
	acquired := make(chan struct{})
	go func() {
		gate, err := limiter.acquire(ctx, 0, 1)
		if err == nil {
			limiter.release(0, 1, gate)
		}
		close(acquired)
	}()
	select {
	case <-acquired:
		t.Fatal("held gate must block a second acquire on the same target")
	case <-time.After(20 * time.Millisecond):
	}
	cancel()
	<-acquired

	for i, gate := range held {
		limiter.release(int64(i), 1, gate)
	}

	limiter.mu.Lock()
	retained := len(limiter.gates)
	limiter.mu.Unlock()
	// Free gates are only pruned while over the soft cap: the map must have
	// shrunk from 1044 and stay bounded, and a released target must be
	// immediately reusable.
	if retained >= reactionGateMaxEntries+20 || retained > reactionGateMaxEntries {
		t.Fatalf("released gates must be collectable, map holds %d", retained)
	}
	gate, err := limiter.acquire(context.Background(), 0, 1)
	if err != nil {
		t.Fatalf("released target must be reusable: %v", err)
	}
	limiter.release(0, 1, gate)

	// Fill the cooldown map beyond the soft cap with unexpired deadlines:
	// none may be dropped.
	now := time.Now()
	limiter.mu.Lock()
	limiter.cooldowns = make(map[int64]time.Time)
	for i := 0; i < reactionCooldownMaxEntries+10; i++ {
		limiter.cooldowns[int64(i)] = now.Add(time.Hour)
	}
	limiter.mu.Unlock()
	limiter.penalizeChat(-1, time.Minute)

	limiter.mu.Lock()
	count := len(limiter.cooldowns)
	penalized := limiter.cooldowns[-1]
	limiter.mu.Unlock()
	if count < reactionCooldownMaxEntries+10 {
		t.Fatalf("unexpired cooldowns must not be dropped, map holds %d", count)
	}
	if !penalized.After(now.Add(time.Minute)) {
		t.Fatalf("full retry deadline must be preserved (with pad), got %v", penalized)
	}

	// Expired entries are collectable.
	limiter.mu.Lock()
	for i := 0; i < 10; i++ {
		limiter.cooldowns[int64(i)] = now.Add(-time.Hour)
	}
	limiter.mu.Unlock()
	limiter.penalizeChat(-2, time.Minute)
	limiter.mu.Lock()
	dropped := 0
	for i := 0; i < 10; i++ {
		if _, ok := limiter.cooldowns[int64(i)]; !ok {
			dropped++
		}
	}
	limiter.mu.Unlock()
	if dropped == 0 {
		t.Fatal("expired cooldowns must be collectable once over the cap")
	}
}

// TestReactionLimiterPrunesExpiredCooldownsOnOrdinarySends proves expired
// cooldown entries are collected by the normal reservation path too, not
// only when a 429 penalizes the chat: without it the map would grow by one
// entry per chat ever reacted to, for the process lifetime.
func TestReactionLimiterPrunesExpiredCooldownsOnOrdinarySends(t *testing.T) {
	t.Parallel()

	var limiter reactionLimiter
	limiter.spacing = time.Millisecond
	limiter.maxWait = 50 * time.Millisecond

	now := time.Now()
	limiter.mu.Lock()
	limiter.cooldowns = make(map[int64]time.Time)
	for i := 0; i < reactionCooldownMaxEntries+5; i++ {
		limiter.cooldowns[int64(i)] = now.Add(-time.Hour) // all expired
	}
	limiter.mu.Unlock()

	// An ordinary spaced send in a fresh chat must collect them.
	if err := limiter.awaitChat(context.Background(), -1); err != nil {
		t.Fatalf("awaitChat: %v", err)
	}

	limiter.mu.Lock()
	count := len(limiter.cooldowns)
	reserved := limiter.cooldowns[-1]
	limiter.mu.Unlock()
	if count > reactionCooldownMaxEntries {
		t.Fatalf("expired cooldowns must be pruned by ordinary sends, map holds %d", count)
	}
	if reserved.IsZero() {
		t.Fatal("the fresh chat's reservation must be recorded")
	}
}

// TestSetReactionGateAcquisitionIsBounded proves waiting for a busy target
// gate respects the per-call wait bound: a stalled holder must not stall the
// run's remaining tools and response indefinitely, and a refused wait must
// not consume the run attempt.
func TestSetReactionGateAcquisitionIsBounded(t *testing.T) {
	t.Parallel()

	sender := &stubActionSender{}
	runtime := newSetReactionRuntime(t, nil, sender)
	scope := state.ConversationScope{ChatID: 1100}

	// Hold the target gate from another run. No defer: the explicit release
	// below must be the only one, a double release would deadlock.
	gate, err := runtime.reactions.acquire(context.Background(), 1100, 42)
	if err != nil {
		t.Fatalf("acquire holder gate: %v", err)
	}

	start := time.Now()
	result := callSetReaction(t, runtime, reactionCallCtx(scope, state.Message{FromID: 11, ChatID: 1100, MessageID: 42}), `{"emoji":"👍"}`)
	if elapsed := time.Since(start); elapsed > time.Second {
		runtime.reactions.release(1100, 42, gate)
		t.Fatalf("bounded wait must refuse promptly, took %v", elapsed)
	}
	if code := reactionErrorCode(t, result); code != "target_busy" {
		runtime.reactions.release(1100, 42, gate)
		t.Fatalf("expected target_busy, got %q", code)
	}
	if sender.reactionCallCount() != 0 {
		runtime.reactions.release(1100, 42, gate)
		t.Fatal("a refused gate wait must not send")
	}

	// The refusal consumed nothing: once the gate frees, the same run
	// proceeds.
	runtime.reactions.release(1100, 42, gate)
	result = callSetReaction(t, runtime, reactionCallCtx(scope, state.Message{FromID: 11, ChatID: 1100, MessageID: 42}), `{"emoji":"👍"}`)
	if result.Status != execution.StatusOK {
		t.Fatalf("proceeding call after release must succeed, got %+v", result.Data)
	}
	if sender.reactionCallCount() != 1 {
		t.Fatalf("unexpected call count: %d", sender.reactionCallCount())
	}
}

// TestSetReactionRejectsSpentAttemptBeforeWaiting proves a repeated call in
// one run fails immediately with already_reacted instead of first waiting on
// the target gate and the chat cooldown it can never use — the wait would
// burn processing time the model needs for its follow-up answer.
func TestSetReactionRejectsSpentAttemptBeforeWaiting(t *testing.T) {
	t.Parallel()

	sender := &stubActionSender{}
	runtime := newSetReactionRuntime(t, nil, sender)
	scope := state.ConversationScope{ChatID: 1200}
	callCtx := reactionCallCtx(scope, state.Message{FromID: 11, ChatID: 1200, MessageID: 42})

	if result := callSetReaction(t, runtime, callCtx, `{"emoji":"👍"}`); result.Status != execution.StatusOK {
		t.Fatalf("first reaction must succeed, got %+v", result.Data)
	}

	// Put the chat into a cooldown beyond the per-call wait bound: if the
	// handler still reached the wait, it would refuse with rate_limited
	// rather than already_reacted, proving the wrong order.
	runtime.reactions.mu.Lock()
	runtime.reactions.cooldowns[1200] = time.Now().Add(time.Hour)
	runtime.reactions.mu.Unlock()

	start := time.Now()
	result := callSetReaction(t, runtime, callCtx, `{"emoji":"🔥"}`)
	if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
		t.Fatalf("spent attempt must be rejected immediately, took %v", elapsed)
	}
	if code := reactionErrorCode(t, result); code != "already_reacted" {
		t.Fatalf("expected already_reacted, got %q", code)
	}
	if sender.reactionCallCount() != 1 {
		t.Fatalf("unexpected call count: %d", sender.reactionCallCount())
	}
}

// --- runtime flows ---

func reactionRuntimeFlow(t *testing.T, sender *stubActionSender, recorder *recordingDispatcher) *Runtime {
	t.Helper()

	runtime := newSetReactionRuntime(t, nil, sender)
	runtime.config.Dispatcher = recorder

	return runtime
}

func TestReplyWithToolsReactionOnlyCompletesEmpty(t *testing.T) {
	t.Parallel()

	recorder := &recordingDispatcher{}
	sender := &stubActionSender{}
	runtime := reactionRuntimeFlow(t, sender, recorder)
	stub := runtime.llm.(*stubLLM)
	stub.responses = []llm.Response{
		{Message: llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{
			{ID: "call-1", Name: "set_reaction", Arguments: json.RawMessage(`{"emoji":"😂"}`)},
		}}},
		{Message: llm.Message{Role: llm.RoleAssistant}},
	}

	result, err := runtime.ReplyWithTools(context.Background(), ChatRequest{
		Scope:          state.ConversationScope{ChatID: 1},
		PromptScope:    llm.PromptScope{ChatID: 1},
		ReplyContext:   llm.ChatReplyContext{UserMessage: llm.TextMessage(llm.RoleUser, "lol")},
		RequestMessage: state.Message{FromID: 11, ChatID: 1, MessageID: 42},
	})
	if err != nil {
		t.Fatalf("reaction-only run must succeed: %v", err)
	}
	if result.Reply != "" {
		t.Fatalf("reaction-only run must complete without text, got %q", result.Reply)
	}
	if sender.reactionCallCount() != 1 {
		t.Fatalf("unexpected API calls: %d", sender.reactionCallCount())
	}
	if got := eventsOf[execution.RunCompleted](recorder.events); len(got) != 1 || got[0].ReplyLength != 0 {
		t.Fatalf("expected one successful empty RunCompleted, got %+v", got)
	}
}

func TestReplyWithToolsReactionPlusText(t *testing.T) {
	t.Parallel()

	recorder := &recordingDispatcher{}
	sender := &stubActionSender{}
	runtime := reactionRuntimeFlow(t, sender, recorder)
	stub := runtime.llm.(*stubLLM)
	stub.responses = []llm.Response{
		{Message: llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{
			{ID: "call-1", Name: "set_reaction", Arguments: json.RawMessage(`{"emoji":"👍"}`)},
		}}},
		{Message: llm.TextMessage(llm.RoleAssistant, "Here is the full answer.")},
	}

	result, err := runtime.ReplyWithTools(context.Background(), ChatRequest{
		Scope:          state.ConversationScope{ChatID: 1},
		PromptScope:    llm.PromptScope{ChatID: 1},
		ReplyContext:   llm.ChatReplyContext{UserMessage: llm.TextMessage(llm.RoleUser, "nice work")},
		RequestMessage: state.Message{FromID: 11, ChatID: 1, MessageID: 42},
	})
	if err != nil {
		t.Fatalf("reaction+text run must succeed: %v", err)
	}
	if result.Reply != "Here is the full answer." {
		t.Fatalf("unexpected reply: %q", result.Reply)
	}
	if sender.reactionCallCount() != 1 {
		t.Fatalf("unexpected API calls: %d", sender.reactionCallCount())
	}
}

func TestReplyWithToolsFailedReactionCannotAuthorizeEmptyCompletion(t *testing.T) {
	t.Parallel()

	sender := &stubActionSender{reactionErr: &telegoapi.Error{ErrorCode: 400, Description: "Bad Request: REACTION_INVALID"}}
	runtime := reactionRuntimeFlow(t, sender, &recordingDispatcher{})
	stub := runtime.llm.(*stubLLM)
	stub.responses = []llm.Response{
		{Message: llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{
			{ID: "call-1", Name: "set_reaction", Arguments: json.RawMessage(`{"emoji":"👍"}`)},
		}}},
		{Message: llm.Message{Role: llm.RoleAssistant}},
	}

	result, err := runtime.ReplyWithTools(context.Background(), ChatRequest{
		Scope:          state.ConversationScope{ChatID: 1},
		PromptScope:    llm.PromptScope{ChatID: 1},
		ReplyContext:   llm.ChatReplyContext{UserMessage: llm.TextMessage(llm.RoleUser, "hello")},
		RequestMessage: state.Message{FromID: 11, ChatID: 1, MessageID: 42},
	})
	if err == nil {
		t.Fatal("a failed reaction must not authorize silent completion")
	}
	if result.Reply != "" {
		t.Fatalf("failed run must not carry a reply, got %q", result.Reply)
	}
}

func TestReplyWithToolsGeneratesRunIDForReactionEvent(t *testing.T) {
	t.Parallel()

	recorder := &recordingDispatcher{}
	sender := &stubActionSender{}
	runtime := reactionRuntimeFlow(t, sender, recorder)
	stub := runtime.llm.(*stubLLM)
	stub.responses = []llm.Response{
		{Message: llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{
			{ID: "call-1", Name: "set_reaction", Arguments: json.RawMessage(`{"emoji":"🎉"}`)},
		}}},
		{Message: llm.TextMessage(llm.RoleAssistant, "Congrats!")},
	}

	// RunID deliberately omitted: the runtime generates the effective one.
	_, err := runtime.ReplyWithTools(context.Background(), ChatRequest{
		Scope:          state.ConversationScope{ChatID: 1},
		PromptScope:    llm.PromptScope{ChatID: 1},
		ReplyContext:   llm.ChatReplyContext{UserMessage: llm.TextMessage(llm.RoleUser, "yay")},
		RequestMessage: state.Message{FromID: 11, ChatID: 1, MessageID: 42},
	})
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}

	starts := eventsOf[execution.RunStarted](recorder.events)
	reactions := eventsOf[execution.BotReactionSet](recorder.events)
	if len(starts) != 1 || len(reactions) != 1 {
		t.Fatalf("expected one RunStarted and one BotReactionSet, got %d/%d", len(starts), len(reactions))
	}
	if starts[0].RunID == "" {
		t.Fatal("effective run ID must be generated when omitted")
	}
	if reactions[0].RunID != starts[0].RunID {
		t.Fatalf("reaction event must correlate with the run: %q vs %q", reactions[0].RunID, starts[0].RunID)
	}
	if tu.ID(1).ID != 1 {
		t.Fatal("telegoutil sanity check failed")
	}
}

// Compile-time interface assertion lives with the fake's consumers.
var _ TelegramActionSender = (*stubActionSender)(nil)
