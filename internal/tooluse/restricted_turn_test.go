package tooluse

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	tg "github.com/mymmrac/telego"

	"telegram-ollama-reply-bot/internal/execution"
	"telegram-ollama-reply-bot/internal/llm"
	"telegram-ollama-reply-bot/internal/state"
	"telegram-ollama-reply-bot/internal/state/memory"
)

// memoryConversations builds the volatile conversation store for the
// restricted-run tests.
func memoryConversations(t *testing.T) state.ConversationStore {
	t.Helper()

	return memory.New(memory.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil))).Conversations()
}

// restrictedReactionRequest builds the restricted reaction-only ChatRequest
// the proactive React path uses.
func restrictedReactionRequest() ChatRequest {
	return ChatRequest{
		Scope:                   state.ConversationScope{ChatID: 1},
		PromptScope:             llm.PromptScope{ChatID: 1},
		ReplyContext:            llm.ChatReplyContext{UserMessage: llm.TextMessage(llm.RoleUser, "nice message")},
		RequestMessage:          state.Message{FromID: 11, ChatID: 1, MessageID: 42},
		AllowedTools:            []string{"set_reaction"},
		CompleteAfterSideEffect: true,
	}
}

// restrictedRuntime wires the standard registry with a stub LLM for the
// restricted-run tests.
func restrictedRuntime(t *testing.T, sender *stubActionSender) (*Runtime, *stubLLM) {
	t.Helper()

	stub := &stubLLM{}
	runtime := New(
		stub,
		memoryConversations(t),
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

	return runtime, stub
}

// TestRestrictedRunAdvertisesOnlyAllowedTools pins the advertisement and
// policy restriction: only the allowed tool reaches the model.
func TestRestrictedRunAdvertisesOnlyAllowedTools(t *testing.T) {
	runtime, stub := restrictedRuntime(t, &stubActionSender{})
	stub.responses = []llm.Response{
		{Message: llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{
			{ID: "call-1", Name: "set_reaction", Arguments: json.RawMessage(`{"emoji":"🎉"}`)},
		}}},
	}

	_, err := runtime.ReplyWithTools(context.Background(), restrictedReactionRequest())
	if err != nil {
		t.Fatalf("restricted run: %v", err)
	}

	names := make([]string, 0, len(stub.lastBuildTools))
	for _, tool := range stub.lastBuildTools {
		names = append(names, tool.Name)
	}
	if len(names) != 1 || names[0] != "set_reaction" {
		t.Fatalf("restricted run must advertise exactly set_reaction, got %v", names)
	}
	if !strings.Contains(stub.lastBuildPolicy, "set_reaction") {
		t.Fatalf("tool policy must list the allowed tool: %q", stub.lastBuildPolicy)
	}
	for _, forbidden := range []string{"send_dice", "search_history", "say_nothing"} {
		if strings.Contains(stub.lastBuildPolicy, "- "+forbidden+": ") {
			t.Fatalf("tool policy must not list %q: %q", forbidden, stub.lastBuildPolicy)
		}
	}
}

// TestRestrictedRunRejectsDisallowedToolCall pins the execution allowlist:
// a hallucinated non-allowed call takes the unknown-tool path — error result
// recorded, no side effect, and the run still completes on the allowed one.
func TestRestrictedRunRejectsDisallowedToolCall(t *testing.T) {
	sender := &stubActionSender{}
	runtime, stub := restrictedRuntime(t, sender)
	stub.responses = []llm.Response{
		{Message: llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{
			{ID: "call-1", Name: "send_dice", Arguments: json.RawMessage(`{}`)},
			{ID: "silence", Name: "say_nothing", Arguments: json.RawMessage(`{}`)},
			{ID: "call-2", Name: "set_reaction", Arguments: json.RawMessage(`{"emoji":"🎉"}`)},
		}}},
	}

	result, err := runtime.ReplyWithTools(context.Background(), restrictedReactionRequest())
	if err != nil {
		t.Fatalf("restricted run with mixed batch: %v", err)
	}

	// The disallowed call was rejected without any Telegram side effect.
	if sender.lastDiceParams != nil {
		t.Fatalf("disallowed tool must not execute")
	}
	if len(result.ToolCalls) != 3 {
		t.Fatalf("expected both calls recorded, got %d", len(result.ToolCalls))
	}
	if result.ToolCalls[0].Name != "send_dice" || result.ToolCalls[0].Status != execution.StatusError {
		t.Fatalf("disallowed call must record an error, got %+v", result.ToolCalls[0])
	}
	if result.ToolCalls[1].Name != "say_nothing" || result.ToolCalls[1].Status != execution.StatusError || result.IntentionalSilence {
		t.Fatalf("disallowed silence must not terminate, got %+v", result)
	}
	if result.ToolCalls[2].Name != "set_reaction" || result.ToolCalls[2].Status != execution.StatusOK {
		t.Fatalf("allowed call must succeed, got %+v", result.ToolCalls[2])
	}
	// Completion happened on the successful allowed call: no second
	// generation request.
	if stub.buildCount != 1 {
		t.Fatalf("restricted completion must skip the next generation, got %d builds", stub.buildCount)
	}
}

// TestRestrictedRunModelSeesRejection pins that in a restricted run that
// continues (no completion yet), the rejected call reaches the model as the
// unknown-tool error it must learn from.
func TestRestrictedRunModelSeesRejection(t *testing.T) {
	sender := &stubActionSender{}
	runtime, stub := restrictedRuntime(t, sender)
	request := restrictedReactionRequest()
	request.CompleteAfterSideEffect = false // keep the loop running
	stub.responses = []llm.Response{
		{Message: llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{
			{ID: "call-1", Name: "send_dice", Arguments: json.RawMessage(`{}`)},
		}}},
		{Message: llm.TextMessage(llm.RoleAssistant, "understood, stopping")},
	}

	result, err := runtime.ReplyWithTools(context.Background(), request)
	if err != nil {
		t.Fatalf("restricted run: %v", err)
	}
	if result.ToolCalls[0].Status != execution.StatusError {
		t.Fatalf("disallowed call must record an error, got %+v", result.ToolCalls[0])
	}
	var rejection string
	for _, message := range stub.lastBuildExtras {
		if message.ToolCallID == "call-1" {
			rejection = message.Text()
		}
	}
	if !strings.Contains(rejection, "unknown tool") {
		t.Fatalf("rejection must take the unknown-tool path, got %q", rejection)
	}
	if sender.lastDiceParams != nil {
		t.Fatalf("disallowed tool must not execute")
	}
}

// TestRestrictedRunCompletesOnSuccessfulReaction pins the completion
// contract: one generation round, no empty-reply fallback round, usage and
// records preserved, no text published.
func TestRestrictedRunCompletesOnSuccessfulReaction(t *testing.T) {
	sender := &stubActionSender{}
	runtime, stub := restrictedRuntime(t, sender)
	stub.responses = []llm.Response{
		{Message: llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{
			{ID: "call-1", Name: "set_reaction", Arguments: json.RawMessage(`{"emoji":"🎉"}`)},
		}},
			Usage: llm.TokenUsage{TotalTokens: 9}},
		// A further model turn must never be requested; if it wrongly were,
		// this response would surface as the run's text.
		{Message: llm.TextMessage(llm.RoleAssistant, "fallback text that must never be produced")},
	}

	result, err := runtime.ReplyWithTools(context.Background(), restrictedReactionRequest())
	if err != nil {
		t.Fatalf("restricted run: %v", err)
	}
	if result.Reply != "" {
		t.Fatalf("restricted completion must produce no text, got %q", result.Reply)
	}
	if stub.buildCount != 1 {
		t.Fatalf("completion must not trigger another generation, got %d builds", stub.buildCount)
	}
	if result.Usage == nil || result.Usage.TotalTokens != 9 {
		t.Fatalf("usage must be preserved, got %+v", result.Usage)
	}
	if len(result.ToolCalls) != 1 || result.ToolCalls[0].Status != execution.StatusOK {
		t.Fatalf("reaction record must be preserved, got %+v", result.ToolCalls)
	}
	if sender.reactionCallCount() != 1 {
		t.Fatalf("unexpected reaction API calls: %d", sender.reactionCallCount())
	}
}

// TestRestrictedRunFailedReactionConsumesAttempt pins that a failed reaction
// does not complete the run: the loop continues, a model retry hits the
// per-run attempt guard (already_reacted, one API call total), and the run
// ends when the model stops calling tools.
func TestRestrictedRunFailedReactionConsumesAttempt(t *testing.T) {
	sender := &stubActionSender{reactionErr: errors.New("telegram rejected the reaction")}
	runtime, stub := restrictedRuntime(t, sender)
	stub.responses = []llm.Response{
		{Message: llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{
			{ID: "call-1", Name: "set_reaction", Arguments: json.RawMessage(`{"emoji":"🎉"}`)},
		}}},
		{Message: llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{
			{ID: "call-2", Name: "set_reaction", Arguments: json.RawMessage(`{"emoji":"🎉"}`)},
		}}},
		{Message: llm.TextMessage(llm.RoleAssistant, "giving up quietly")},
	}

	result, err := runtime.ReplyWithTools(context.Background(), restrictedReactionRequest())
	if err != nil {
		t.Fatalf("restricted run: %v", err)
	}

	// Exactly one API attempt was spent; the retry hit the guard without a
	// second send.
	if sender.reactionCallCount() != 1 {
		t.Fatalf("failed attempt must be consumed once, got %d API calls", sender.reactionCallCount())
	}
	if len(result.ToolCalls) != 2 {
		t.Fatalf("both attempts must be recorded, got %d", len(result.ToolCalls))
	}
	for _, record := range result.ToolCalls {
		if record.Status != execution.StatusError {
			t.Fatalf("both reaction attempts failed, got %+v", record)
		}
	}
	// The run only ended when the model stopped calling tools; the text
	// comes back for the caller to discard.
	if result.Reply != "giving up quietly" {
		t.Fatalf("loop must end with the model's final response, got %q", result.Reply)
	}
	if stub.buildCount != 3 {
		t.Fatalf("expected three generation rounds, got %d", stub.buildCount)
	}
	// The retry must have seen the already_reacted error, not a fresh send.
	var retryResult string
	for _, message := range stub.lastBuildExtras {
		if message.ToolCallID == "call-2" {
			retryResult = message.Text()
		}
	}
	if !strings.Contains(retryResult, "already_reacted") {
		t.Fatalf("retry must see already_reacted, got %q", retryResult)
	}
}

// TestRestrictedRunTextOnlyResponseEndsRun pins that a text-only response in
// a restricted run ends the turn without any side effect; the text is
// returned for the caller to discard (never published by the runtime).
func TestRestrictedRunTextOnlyResponseEndsRun(t *testing.T) {
	sender := &stubActionSender{}
	runtime, stub := restrictedRuntime(t, sender)
	stub.responses = []llm.Response{
		{Message: llm.TextMessage(llm.RoleAssistant, "I would react, but here is text instead")},
	}

	result, err := runtime.ReplyWithTools(context.Background(), restrictedReactionRequest())
	if err != nil {
		t.Fatalf("restricted run: %v", err)
	}
	if result.Reply != "I would react, but here is text instead" {
		t.Fatalf("text-only run must return the response for the caller to discard, got %q", result.Reply)
	}
	if sender.reactionCallCount() != 0 {
		t.Fatalf("no reaction may be attempted, got %d", sender.reactionCallCount())
	}
	if stub.buildCount != 1 {
		t.Fatalf("run must end after the text response, got %d builds", stub.buildCount)
	}
}

// TestRestrictedRunIterationLimitStillApplies pins that MaxIterations bounds
// a restricted run whose model keeps calling the (failing) allowed tool.
func TestRestrictedRunIterationLimitStillApplies(t *testing.T) {
	sender := &stubActionSender{reactionErr: errors.New("backend down")}
	runtime, stub := restrictedRuntime(t, sender)
	runtime.config.MaxIterations = 3

	call := llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{
		{ID: "call", Name: "set_reaction", Arguments: json.RawMessage(`{"emoji":"🎉"}`)},
	}}
	stub.responses = []llm.Response{
		{Message: call}, {Message: call}, {Message: call},
		// Extra responses must never be consumed: the loop caps first.
		{Message: llm.TextMessage(llm.RoleAssistant, "unreachable")},
	}

	_, err := runtime.ReplyWithTools(context.Background(), restrictedReactionRequest())
	if !errors.Is(err, ErrToolLoopLimitReached) {
		t.Fatalf("iteration limit must still bound restricted runs, got %v", err)
	}
	if stub.buildCount != 3 {
		t.Fatalf("expected exactly MaxIterations builds, got %d", stub.buildCount)
	}
}

// TestUnrestrictedRunIgnoresCompletionFlag pins that the completion flag
// alone cannot change an ordinary turn: without an allowlist the loop
// semantics are the historical ones (empty-reply fallback after a
// successful side effect, not an immediate return).
func TestUnrestrictedRunIgnoresCompletionFlag(t *testing.T) {
	stub := &stubLLM{
		responses: []llm.Response{
			{Message: llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{
				{ID: "call-1", Name: "send_dice", Arguments: json.RawMessage(`{}`)},
			}}},
			{Message: llm.TextMessage(llm.RoleAssistant, "and here is the text")},
		},
	}
	runtime := New(
		stub,
		memoryConversations(t),
		&stubExtractor{},
		nil,
		&stubActionSender{diceMessage: &tg.Message{MessageID: 55, Dice: &tg.Dice{Emoji: "🎲", Value: 6}}},
		nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		Config{MaxIterations: 6},
	)

	result, err := runtime.ReplyWithTools(context.Background(), ChatRequest{
		Scope:                   state.ConversationScope{ChatID: 1},
		PromptScope:             llm.PromptScope{ChatID: 1},
		ReplyContext:            llm.ChatReplyContext{UserMessage: llm.TextMessage(llm.RoleUser, "roll")},
		CompleteAfterSideEffect: true, // misused flag without an allowlist
	})
	if err != nil {
		t.Fatalf("ordinary run: %v", err)
	}
	if result.Reply != "and here is the text" {
		t.Fatalf("ordinary loop semantics must be unchanged, got %q", result.Reply)
	}
	if stub.buildCount != 2 {
		t.Fatalf("ordinary run must run to the final text, got %d builds", stub.buildCount)
	}
}

// TestAllowedDefinitionsNilKeepsAll pins the intersection helper directly.
func TestAllowedDefinitionsNilKeepsAll(t *testing.T) {
	runtime := newRegistryForTest(t)
	all := runtime.registry.DefaultDefinitions()

	if got := allowedDefinitions(all, nil); !slices.Equal(namesOf(got), namesOf(all)) {
		t.Fatalf("nil allowlist must keep the full set")
	}

	restricted := allowedDefinitions(all, []string{"set_reaction", "not_a_real_tool"})
	if len(restricted) != 1 || restricted[0].Name != "set_reaction" {
		t.Fatalf("allowlist must intersect and keep registry order, got %v", namesOf(restricted))
	}

	if got := allowedDefinitions(all, []string{}); len(got) != 0 {
		t.Fatalf("empty allowlist must restrict to nothing, got %v", namesOf(got))
	}
}
