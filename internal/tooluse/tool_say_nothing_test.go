package tooluse

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"telegram-ollama-reply-bot/internal/execution"
	"telegram-ollama-reply-bot/internal/llm"
	"telegram-ollama-reply-bot/internal/state"
)

func silenceCall(id string) llm.ToolCall {
	return llm.ToolCall{ID: id, Name: "say_nothing", Arguments: json.RawMessage(`{}`)}
}

func TestSayNothingArguments(t *testing.T) {
	for _, args := range []string{"", "  ", "null", "{}", " { } "} {
		t.Run("valid/"+args, func(t *testing.T) {
			result, err := sayNothingHandler(context.Background(), CallContext{}, json.RawMessage(args))
			if err != nil || result.Status != execution.StatusOK || !result.endTurn {
				t.Fatalf("result = %+v, err = %v", result, err)
			}
			if got := marshalResult(result, 100); got != `{"status":"ok"}` {
				t.Fatalf("control flow must not appear on the wire: %s", got)
			}
		})
	}
	for _, args := range []string{`{"text":"discard"}`, `{"unused":null}`, `[]`, `0`, `false`, `""`, `{`, `{} {}`, `null true`} {
		t.Run("invalid/"+args, func(t *testing.T) {
			result, err := sayNothingHandler(context.Background(), CallContext{}, json.RawMessage(args))
			if err != nil || result.Status != execution.StatusError || result.endTurn {
				t.Fatalf("invalid arguments terminated: %+v, %v", result, err)
			}
		})
	}
}

func TestSayNothingRegistry(t *testing.T) {
	runtime := newRegistryForTest(t)
	def, ok := runtime.registry.Lookup("say_nothing")
	if !ok || def.SideEffecting || def.EffectiveVisibility() != VisibilityInternal || def.InvocationPolicy != InvocationPolicyDiscretionary {
		t.Fatalf("unexpected definition: %+v", def)
	}
	if def.Description != "End the current turn without sending additional text. Previously completed tool actions remain unaffected." {
		t.Fatalf("unexpected description: %s", def.Description)
	}
	if string(def.Parameters) != `{"type":"object","properties":{},"additionalProperties":false}` {
		t.Fatalf("unexpected schema: %s", def.Parameters)
	}
	if !strings.HasPrefix(setReactionDescription(false), "React to the triggering message using one available emoji. You may reply afterwards or call say_nothing.") {
		t.Fatal("reaction description must offer both continuations")
	}
	if strings.Contains(setReactionDescription(false), "substantive") || strings.Contains(setReactionDescription(false), "unnecessarily") {
		t.Fatal("reaction description must not discourage reaction-only responses")
	}
}

func TestSayNothingStopsBatchAndGeneration(t *testing.T) {
	for _, before := range []int{0, 1, 2} {
		for _, separateRound := range []bool{false, true} {
			t.Run(fmt.Sprintf("before=%d/separate=%t", before, separateRound), func(t *testing.T) {
				var effects []string
				var calls []llm.ToolCall
				for i := 0; i < before; i++ {
					calls = append(calls, llm.ToolCall{ID: string(rune('a' + i)), Name: "effect", Arguments: json.RawMessage(`{}`)})
				}
				batch := append([]llm.ToolCall(nil), calls...)
				if separateRound {
					batch = nil
				}
				batch = append(batch, silenceCall("stop"), llm.ToolCall{ID: "skipped", Name: "effect"}, silenceCall("duplicate"))
				response := llm.Response{Message: llm.TextMessage(llm.RoleAssistant, "unsent text"), Usage: llm.TokenUsage{PromptTokens: 3, CompletionTokens: 2, TotalTokens: 5, Cost: 0.25}}
				response.Message.ToolCalls = batch
				stub := &stubLLM{responses: []llm.Response{response}}
				if separateRound {
					stub.responses = append([]llm.Response{{Message: llm.Message{Role: llm.RoleAssistant, ToolCalls: calls}, Usage: response.Usage}}, response)
					// An empty first batch would finish before say_nothing.
					if before == 0 {
						stub.responses = []llm.Response{response}
					}
				}
				rounds := len(stub.responses)
				stub.responses = append(stub.responses, llm.Response{Message: llm.TextMessage(llm.RoleAssistant, "must not request")})
				recorder := &recordingDispatcher{}
				runtime := newEventRuntime(t, stub, recorder)
				runtime.config.MaxIterations = rounds // terminal success on the final allowed iteration
				runtime.registry.Register(Definition{Name: "effect", SideEffecting: true, Handler: func(_ context.Context, _ CallContext, _ json.RawMessage) (toolResult, error) {
					effects = append(effects, "done")

					return toolResult{Status: execution.StatusOK}, nil
				}})
				result, err := runtime.ReplyWithTools(context.Background(), eventRequest())
				if err != nil || !result.IntentionalSilence || result.Reply != "" {
					t.Fatalf("result = %+v, err = %v", result, err)
				}
				if stub.buildCount != rounds || len(stub.generateModels) != rounds || len(effects) != before {
					t.Fatalf("builds=%d generations=%d effects=%v", stub.buildCount, len(stub.generateModels), effects)
				}
				wantUsage := llm.TokenUsage{PromptTokens: 3 * rounds, CompletionTokens: 2 * rounds, TotalTokens: 5 * rounds, Cost: 0.25 * float64(rounds)}
				if result.Usage == nil || *result.Usage != wantUsage {
					t.Fatalf("usage = %+v, want %+v", result.Usage, wantUsage)
				}
				if len(result.ToolCalls) != before+1 || result.ToolCalls[before].CallID != "stop" || result.ToolCalls[before].SideEffecting {
					t.Fatalf("records = %+v", result.ToolCalls)
				}
				events := recorder.snapshot()
				starts := eventsOf[execution.ToolStarted](events)
				dones := eventsOf[execution.ToolCompleted](events)
				if len(starts) != before+1 || len(dones) != before+1 {
					t.Fatalf("unexpected executed event counts: %d / %d", len(starts), len(dones))
				}
				for i, record := range result.ToolCalls {
					if starts[i].CallID != record.CallID || dones[i].CallID != record.CallID || dones[i].Status != execution.StatusOK {
						t.Fatalf("events do not match executed prefix: %+v / %+v", starts, dones)
					}
				}
				completed := eventsOf[execution.RunCompleted](events)
				if len(completed) != 1 || completed[0].ReplyLength != 0 || len(eventsOf[execution.RunFailed](events)) != 0 {
					t.Fatalf("unexpected lifecycle: %+v", events)
				}
				if _, ok := events[len(events)-1].(execution.RunCompleted); !ok {
					t.Fatal("completion must follow the terminal tool events")
				}
			})
		}
	}
}

func TestRejectedSayNothingContinuesLoop(t *testing.T) {
	for _, kind := range []string{"invalid", "disallowed", "unregistered"} {
		t.Run(kind, func(t *testing.T) {
			call := silenceCall("stop")
			if kind == "invalid" {
				call.Arguments = json.RawMessage(`{"text":"x"}`)
			}
			stub := &stubLLM{responses: []llm.Response{
				{Message: llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{call}}},
				{Message: llm.TextMessage(llm.RoleAssistant, "ordinary reply")},
			}}
			runtime := newEventRuntime(t, stub, nil)
			req := eventRequest()
			if kind == "disallowed" {
				req.AllowedTools = []string{"set_reaction"}
			}
			if kind == "unregistered" {
				delete(runtime.registry.definitions, "say_nothing")
			}
			result, err := runtime.ReplyWithTools(context.Background(), req)
			if err != nil || result.IntentionalSilence || result.Reply != "ordinary reply" || stub.buildCount != 2 {
				t.Fatalf("result=%+v err=%v builds=%d", result, err, stub.buildCount)
			}
			if len(result.ToolCalls) != 1 || result.ToolCalls[0].Status != execution.StatusError {
				t.Fatalf("records=%+v", result.ToolCalls)
			}
			if len(stub.lastBuildExtras) != 2 || stub.lastBuildExtras[1].ToolCallID != call.ID || !strings.Contains(stub.lastBuildExtras[1].Text(), `"status":"error"`) {
				t.Fatalf("rejection pairing lost: %+v", stub.lastBuildExtras)
			}
		})
	}
}

type silenceDispatcher struct {
	recordingDispatcher
	hook func(execution.Event)
}

func (d *silenceDispatcher) Publish(event execution.Event) {
	d.recordingDispatcher.Publish(event)
	d.hook(event)
}

func TestSayNothingCancellationBoundary(t *testing.T) {
	for _, after := range []bool{false, true} {
		t.Run(map[bool]string{false: "before", true: "after"}[after], func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			d := &silenceDispatcher{hook: func(event execution.Event) {
				if e, ok := event.(execution.ToolStarted); ok && e.ToolName == "say_nothing" && !after {
					cancel()
				}
				if e, ok := event.(execution.ToolCompleted); ok && e.ToolName == "say_nothing" && after {
					cancel()
				}
			}}
			stub := &stubLLM{responses: []llm.Response{{Message: llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{silenceCall("stop"), {ID: "skipped", Name: "get_current_time"}}}, Usage: llm.TokenUsage{TotalTokens: 7}}}}
			runtime := newEventRuntime(t, stub, d)
			result, err := runtime.ReplyWithTools(ctx, eventRequest())
			if after {
				if err != nil || !result.IntentionalSilence {
					t.Fatalf("accepted completion lost: %+v, %v", result, err)
				}
			} else if !errors.Is(err, context.Canceled) || result.IntentionalSilence {
				t.Fatalf("cancelled call accepted: %+v, %v", result, err)
			}
			if len(result.ToolCalls) != 1 || stub.buildCount != 1 || result.Usage.TotalTokens != 7 {
				t.Fatalf("lost accounting or executed suffix: %+v", result)
			}
			want := execution.StatusError
			if after {
				want = execution.StatusOK
			}
			if result.ToolCalls[0].Status != want {
				t.Fatalf("status=%s, want %s", result.ToolCalls[0].Status, want)
			}
		})
	}
}

type sayNothingDeadlineContext struct {
	context.Context
	expired bool
}

func (c *sayNothingDeadlineContext) Deadline() (time.Time, bool) {
	return time.Unix(1, 0), c.expired
}

func TestSayNothingDeadlineBoundary(t *testing.T) {
	for _, after := range []bool{false, true} {
		t.Run(fmt.Sprintf("after=%t", after), func(t *testing.T) {
			ctx := &sayNothingDeadlineContext{Context: context.Background()}
			d := &silenceDispatcher{hook: func(event execution.Event) {
				if e, ok := event.(execution.ToolStarted); ok && e.ToolName == "say_nothing" && !after {
					ctx.expired = true
				}
				if e, ok := event.(execution.ToolCompleted); ok && e.ToolName == "say_nothing" && after {
					ctx.expired = true
				}
			}}
			stub := &stubLLM{responses: []llm.Response{{Message: llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{silenceCall("stop")}}}}}
			result, err := newEventRuntime(t, stub, d).ReplyWithTools(ctx, eventRequest())
			if after {
				if err != nil || !result.IntentionalSilence {
					t.Fatalf("late deadline overrode acceptance: %+v, %v", result, err)
				}
			} else if !errors.Is(err, context.DeadlineExceeded) || result.IntentionalSilence {
				t.Fatalf("expired deadline accepted: %+v, %v", result, err)
			}
			if stub.buildCount != 1 || len(stub.generateModels) != 1 {
				t.Fatal("deadline must not trigger a followup generation")
			}
		})
	}
}

func TestSayNothingAfterReactionAcrossRounds(t *testing.T) {
	sender := &stubActionSender{}
	recorder := &recordingDispatcher{}
	runtime := reactionRuntimeFlow(t, sender, recorder)
	stub := runtime.llm.(*stubLLM)
	stub.responses = []llm.Response{
		{Message: llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "reaction", Name: "set_reaction", Arguments: []byte(`{"emoji":"🎉"}`)}}}},
		{Message: llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{silenceCall("stop")}}},
	}
	req := eventRequest()
	req.Scope = state.ConversationScope{ChatID: 1}
	req.RequestMessage.ChatID = 1
	runtime.history.AppendMessage(req.Scope, req.RequestMessage)
	result, err := runtime.ReplyWithTools(context.Background(), req)
	if err != nil || !result.IntentionalSilence || result.Reply != "" || stub.buildCount != 2 {
		t.Fatalf("unexpected result: %+v, %v", result, err)
	}
	if sender.reactionCallCount() != 1 || len(result.ToolCalls) != 2 || result.ToolCalls[0].Status != execution.StatusOK {
		t.Fatalf("reaction lost: %+v", result.ToolCalls)
	}
	snapshot := runtime.history.Snapshot(req.Scope)
	if len(snapshot.Messages) != 1 || len(snapshot.Messages[0].Reactions) != 1 {
		t.Fatalf("reaction history lost: %+v", snapshot.Messages)
	}
	if len(stub.lastBuildExtras) != 2 || stub.lastBuildExtras[1].ToolCallID != "reaction" {
		t.Fatalf("ordinary tool pairing changed: %+v", stub.lastBuildExtras)
	}
}

func TestSayNothingDoesNotMaskBackendFailure(t *testing.T) {
	for _, backendErr := range []error{llm.ErrNoChoices, errors.New("backend unavailable"), context.Canceled} {
		t.Run(backendErr.Error(), func(t *testing.T) {
			stub := &stubLLM{generateErr: backendErr, responses: []llm.Response{{Message: llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{silenceCall("unaccepted")}}}}}
			recorder := &recordingDispatcher{}
			result, err := newEventRuntime(t, stub, recorder).ReplyWithTools(context.Background(), eventRequest())
			if !errors.Is(err, backendErr) || result.IntentionalSilence || len(result.ToolCalls) != 0 {
				t.Fatalf("backend failure confused with silence: %+v, %v", result, err)
			}
			if len(eventsOf[execution.RunFailed](recorder.snapshot())) != 1 || len(eventsOf[execution.RunCompleted](recorder.snapshot())) != 0 {
				t.Fatal("backend failure must remain a failed run")
			}
		})
	}
}
