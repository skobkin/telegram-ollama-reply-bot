package tooluse

import (
	"context"
	"errors"
	"testing"
	"time"

	"telegram-ollama-reply-bot/internal/execution"
	"telegram-ollama-reply-bot/internal/llm"
)

// The synchronous event hook advances the deadline after confirmation,
// including the interval before a context timer would publish its error.
type restrictedCompletionContext struct {
	context.Context
	expired    bool
	timerFired bool
}

func (c *restrictedCompletionContext) Deadline() (time.Time, bool) {
	return time.Unix(1, 0), c.expired
}

func (c *restrictedCompletionContext) Err() error {
	if c.timerFired {
		return context.DeadlineExceeded
	}

	return c.Context.Err()
}

func TestRestrictedRunConfirmedReactionContext(t *testing.T) {
	for _, mode := range []string{"cancelled", "deadline", "deadline_before_timer", "cancelled_after_deadline", "failed_reaction_cancelled", "ordinary_cancelled"} {
		t.Run(mode, func(t *testing.T) {
			base, cancel := context.WithCancel(context.Background())
			defer cancel()
			ctx := &restrictedCompletionContext{Context: base}
			sender := &stubActionSender{}
			if mode == "failed_reaction_cancelled" {
				sender.reactionErr = errors.New("Telegram rejected the reaction")
			}
			runtime, stub := restrictedRuntime(t, sender)
			req := restrictedReactionRequest()
			if mode == "ordinary_cancelled" {
				req.AllowedTools = nil
			}
			runtime.history.AppendMessage(req.Scope, req.RequestMessage)
			usage := llm.TokenUsage{PromptTokens: 3, CompletionTokens: 2, TotalTokens: 5, Cost: 0.25}
			stub.responses = []llm.Response{{Message: llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{
				{ID: "reaction", Name: "set_reaction", Arguments: []byte(`{"emoji":"🎉"}`)},
				// Cancellation must not dispatch the remaining batch, nor
				// turn it into a second generation request.
				{ID: "skipped", Name: "set_reaction", Arguments: []byte(`{"emoji":"👍"}`)},
			}}, Usage: usage}}
			d := &silenceDispatcher{hook: func(event execution.Event) {
				if e, ok := event.(execution.ToolCompleted); ok && e.CallID == "reaction" {
					switch mode {
					case "deadline":
						ctx.expired, ctx.timerFired = true, true
					case "deadline_before_timer":
						ctx.expired = true
					case "cancelled_after_deadline":
						ctx.expired = true
						cancel()
					default:
						cancel()
					}
				}
			}}
			runtime.config.Dispatcher = d
			result, err := runtime.ReplyWithTools(ctx, req)
			var wantErr error
			switch mode {
			case "deadline", "deadline_before_timer", "cancelled_after_deadline":
				wantErr = context.DeadlineExceeded
			case "failed_reaction_cancelled", "ordinary_cancelled":
				wantErr = context.Canceled
			}
			if !errors.Is(err, wantErr) || result.Reply != "" || result.IntentionalSilence {
				t.Fatalf("unexpected completion: %+v, err=%v, want %v", result, err, wantErr)
			}
			if result.Usage == nil || *result.Usage != usage || stub.buildCount != 1 || sender.reactionCallCount() != 1 {
				t.Fatalf("lost accounting or extra work: result=%+v, builds=%d, sends=%d", result, stub.buildCount, sender.reactionCallCount())
			}
			status := execution.StatusOK
			if mode == "failed_reaction_cancelled" {
				status = execution.StatusError
			}
			if len(result.ToolCalls) != 1 || result.ToolCalls[0].Status != status || !result.ToolCalls[0].SideEffecting {
				t.Fatalf("incorrect authoritative records: %+v", result.ToolCalls)
			}
			history := runtime.history.Snapshot(req.Scope)
			if len(history.Messages) != 1 || (len(history.Messages[0].Reactions) == 1) != (status == execution.StatusOK) {
				t.Fatalf("incorrect reaction history: %+v", history.Messages)
			}
			events := d.snapshot()
			if len(eventsOf[execution.ToolStarted](events)) != 1 || len(eventsOf[execution.ToolCompleted](events)) != 1 {
				t.Fatal("skipped suffix must produce no invocation events")
			}
			if wantErr == nil {
				if len(eventsOf[execution.RunCompleted](events)) != 1 || len(eventsOf[execution.RunFailed](events)) != 0 {
					t.Fatal("confirmed restricted completion must publish success")
				}
			} else if len(eventsOf[execution.RunFailed](events)) != 1 || len(eventsOf[execution.RunCompleted](events)) != 0 {
				t.Fatal("failed run must retain failure lifecycle events")
			}
		})
	}
}
