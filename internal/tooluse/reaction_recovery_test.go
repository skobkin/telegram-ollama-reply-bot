package tooluse

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"

	"telegram-ollama-reply-bot/internal/execution"
	"telegram-ollama-reply-bot/internal/llm"
	"telegram-ollama-reply-bot/internal/telegram/reactionavailability"

	t "github.com/mymmrac/telego"
	"github.com/mymmrac/telego/telegoapi"
)

func reactionCall(id, emoji string) llm.ToolCall {
	args, _ := json.Marshal(map[string]string{"emoji": emoji})

	return llm.ToolCall{ID: id, Name: "set_reaction", Arguments: args}
}
func toolBatch(calls ...llm.ToolCall) llm.Response {
	return llm.Response{Message: llm.Message{Role: llm.RoleAssistant, ToolCalls: calls}}
}

func TestCorrectedReactionRetryRefreshesEnumAndPreservesSilence(tst *testing.T) {
	rejected := &telegoapi.Error{ErrorCode: 400, Description: "Bad Request: REACTION_INVALID"}
	sender := &stubActionSender{reactionErrs: []error{rejected, nil}}
	r, m := restrictedRuntime(tst, sender)
	g := &availableChat{}
	r.AttachReactionAvailability(reactionavailability.New(g, nil))
	recorder := &recordingDispatcher{}
	r.config.Dispatcher = recorder
	m.responses = []llm.Response{
		toolBatch(reactionCall("bad", "👍")),
		toolBatch(reactionCall("invalid", "not an emoji")),
		toolBatch(reactionCall("fixed", " ❤️ "), reactionCall("duplicate", "🔥"), llm.ToolCall{ID: "quiet", Name: "say_nothing", Arguments: json.RawMessage(`{}`)}),
	}
	req := restrictedReactionRequest()
	req.AllowedTools = nil
	req.CompleteAfterSideEffect = false
	result, err := r.ReplyWithTools(context.Background(), req)
	if err != nil || !result.IntentionalSilence || sender.reactionCallCount() != 2 || g.calls != 2 {
		tst.Fatalf("result=%+v err=%v sends=%d lookups=%d", result, err, sender.reactionCallCount(), g.calls)
	}
	if len(result.ToolCalls) != 5 || result.ToolCalls[0].ErrorClass != ErrorArguments || !result.ToolCalls[0].Retryable || result.ToolCalls[2].SideEffectOutcome != "confirmed" || result.ToolCalls[3].Executed {
		tst.Fatal(result.ToolCalls)
	}
	if len(eventsOf[execution.BotReactionSet](recorder.events)) != 1 {
		tst.Fatal("wrong success events")
	}
	for _, round := range m.buildTools[1:] {
		for _, def := range round {
			if def.Name == "set_reaction" {
				var schema struct {
					Properties struct{ Emoji struct{ Enum []string } }
				}
				if err := json.Unmarshal(def.Parameters, &schema); err != nil {
					tst.Fatal(err)
				}
				if slices.Contains(schema.Properties.Emoji.Enum, "👍") {
					tst.Fatal("rejected emoji reintroduced")
				}
			}
		}
	}
	if sender.lastReactionParams.Reaction[0].(*t.ReactionTypeEmoji).Emoji != "❤" {
		tst.Fatal("presentation variant not canonicalized")
	}
}

func TestThreeOutgoingAttemptsThenUnavailable(tst *testing.T) {
	sender := &stubActionSender{reactionErr: &telegoapi.Error{ErrorCode: 400, Description: "REACTION_INVALID"}}
	r, m := restrictedRuntime(tst, sender)
	m.responses = []llm.Response{toolBatch(reactionCall("one", "👍"), reactionCall("two", "👎"), reactionCall("three", "🔥"), reactionCall("four", "❤")), {Message: llm.TextMessage(llm.RoleAssistant, "done")}}
	req := restrictedReactionRequest()
	req.CompleteAfterSideEffect = false
	result, err := r.ReplyWithTools(context.Background(), req)
	if err != nil || sender.reactionCallCount() != 3 || len(result.ToolCalls) != 4 {
		tst.Fatal(result, err, sender.reactionCallCount())
	}
	if result.ToolCalls[2].Retryable || result.ToolCalls[3].Executed || len(m.lastBuildTools) != 0 {
		tst.Fatal(result.ToolCalls, m.lastBuildTools)
	}
}

func TestAmbiguousAndRateLimitedReactionsCannotRetry(tst *testing.T) {
	for _, cause := range []error{errors.New("lost response"), context.Canceled, &telegoapi.Error{ErrorCode: 429, Parameters: &telegoapi.ResponseParameters{RetryAfter: 1}}, &telegoapi.Error{ErrorCode: 403}} {
		r, m := restrictedRuntime(tst, &stubActionSender{reactionErr: cause})
		m.responses = []llm.Response{toolBatch(reactionCall("one", "👍"), reactionCall("two", "👎")), {Message: llm.TextMessage(llm.RoleAssistant, "done")}}
		req := restrictedReactionRequest()
		req.CompleteAfterSideEffect = false
		result, err := r.ReplyWithTools(context.Background(), req)
		if err != nil || len(result.ToolCalls) != 2 || result.ToolCalls[0].Retryable || result.ToolCalls[1].Executed || len(m.lastBuildTools) != 0 {
			tst.Fatal(cause, result, err)
		}
	}
}
