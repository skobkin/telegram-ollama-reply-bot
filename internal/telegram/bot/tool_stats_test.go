package bot

import (
	"context"
	"errors"
	"testing"

	"telegram-ollama-reply-bot/internal/adminconfig"
	"telegram-ollama-reply-bot/internal/execution"
	"telegram-ollama-reply-bot/internal/llm"
	"telegram-ollama-reply-bot/internal/llmcontext"
	"telegram-ollama-reply-bot/internal/state"
	"telegram-ollama-reply-bot/internal/statistics"
	"telegram-ollama-reply-bot/internal/tooluse"
)

func toolCallResponse(calls ...llm.ToolCall) llm.Response {
	return llm.Response{Message: llm.Message{Role: llm.RoleAssistant, ToolCalls: calls}}
}

func toolStatsTurn(ctx context.Context, t *testing.T, p *proactiveTestBot, trigger llmcontext.TriggerKind, resolved adminconfig.ResolvedChatConfig) {
	t.Helper()
	msg := *pipelineGroupUpdate(21, "hello").Message
	user := state.Message{MessageID: msg.MessageID, ChatID: msg.Chat.ID, FromID: 21, Text: msg.Text}
	p.history.AppendMessage(scopeFromMessage(msg), user)

	p.bot.runConversationTurn(ctx, msg, user, resolved, turnOptions{
		trigger: trigger, errorPolicy: silentErrors, delivery: discardText,
	})
}

func toolRowNamed(t *testing.T, snap statistics.ToolSnapshot, name string) statistics.ToolRowView {
	t.Helper()
	for _, row := range snap.Tools {
		if row.Tool != nil && *row.Tool == name {
			return row
		}
	}
	t.Fatalf("tool %q missing from %+v", name, snap.Tools)

	return statistics.ToolRowView{}
}

func TestConversationTurnRecordsOneToolObservationWithEffectiveModelAndMode(t *testing.T) {
	for _, tc := range []struct {
		name     string
		trigger  llmcontext.TriggerKind
		model    string
		wantMode string
		wantName string
	}{
		{"explicit with override", llmcontext.TriggerMention, "chat-override", "explicit", "chat-override"},
		{"explicit falls back to the route model", llmcontext.TriggerMention, "", "explicit", "route-model"},
		{"proactive", llmcontext.TriggerProactive, "", "proactive", "route-model"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newProactiveTestBot(t, nil)
			p.bot.SetChatRouteModel("route-model") // wired even without proactive participation
			p.llm.responses = []llm.Response{
				toolCallResponse(llm.ToolCall{ID: "1", Name: "get_current_time", Arguments: []byte(`{}`)}),
				{Message: llm.TextMessage(llm.RoleAssistant, "done")},
			}

			toolStatsTurn(context.Background(), t, p, tc.trigger, adminconfig.ResolvedChatConfig{ChatModel: tc.model})

			snap := p.bot.stats.ToolSnapshot(statistics.GlobalScope())
			row := toolRowNamed(t, snap, "get_current_time")
			if row.Counters.Attempts != 1 || row.Counters.AdvertisedTurns != 1 || row.Counters.AdvertisedInvokedTurns != 1 || row.Counters.Succeeded != 1 {
				t.Fatalf("counters %+v", row.Counters)
			}
			// Advertisement is once per turn even though two generation
			// iterations ran.
			if p.llm.buildCount() != 2 {
				t.Fatalf("fixture needs two iterations, got %d", p.llm.buildCount())
			}
			var turns uint64
			for _, v := range snap.Turns {
				turns += v.Turns
				if v.Mode == tc.wantMode && v.Turns != 1 {
					t.Fatalf("turn must be attributed to %s: %+v", tc.wantMode, snap.Turns)
				}
			}
			if turns != 1 {
				t.Fatalf("exactly one observation per turn, got %d", turns)
			}
			if len(snap.Models) != 1 || snap.Models[0].Model == nil || *snap.Models[0].Model != tc.wantName {
				t.Fatalf("effective model attribution: %+v", snap.Models)
			}
		})
	}
}

func TestConversationTurnToolObservationsSurviveFailures(t *testing.T) {
	p := newProactiveTestBot(t, nil)
	p.llm.responses = []llm.Response{
		toolCallResponse(llm.ToolCall{ID: "1", Name: "get_current_time", Arguments: []byte(`{}`)}),
	}
	// The second generation request fails to prepare after one executed
	// tool call (a partially completed turn).
	p.llm.buildErrAfter = 1
	p.llm.buildErr = errors.New("prepare failed")

	toolStatsTurn(context.Background(), t, p, llmcontext.TriggerProactive, adminconfig.ResolvedChatConfig{})

	snap := p.bot.stats.ToolSnapshot(statistics.GlobalScope())
	if row := toolRowNamed(t, snap, "get_current_time"); row.Counters.Attempts != 1 {
		t.Fatalf("tool calls of a failed turn must be retained: %+v", row.Counters)
	}
	var proactive statistics.ToolTurnView
	for _, v := range snap.Turns {
		if v.Mode == "proactive" {
			proactive = v
		}
	}
	if proactive.Turns != 1 || proactive.RunFailures.Generation != 1 {
		t.Fatalf("a generation failure must be classified: %+v", proactive)
	}
}

func TestConversationTurnCancelledTurnKeepsToolStatistics(t *testing.T) {
	p := newProactiveTestBot(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.bot.tools = tooluse.New(p.llm, p.history, nil, nil, apiActionSender{api: p.apiBot}, nil, p.bot.logger, tooluse.Config{
		MaxIterations: 6,
		BotIdentity:   tooluse.BotIdentity{UserID: testBotID.ID, Name: testBotID.FirstName},
		Dispatcher: silenceEventHook(func(event execution.Event) {
			if _, ok := event.(execution.ToolCompleted); ok {
				cancel() // cancelled right after the tool executed
			}
		}),
	})
	p.llm.responses = []llm.Response{
		toolCallResponse(llm.ToolCall{ID: "1", Name: "get_current_time", Arguments: []byte(`{}`)}),
		{Message: llm.TextMessage(llm.RoleAssistant, "unreached")},
	}

	toolStatsTurn(ctx, t, p, llmcontext.TriggerProactive, adminconfig.ResolvedChatConfig{})

	snap := p.bot.stats.ToolSnapshot(statistics.GlobalScope())
	if toolRowNamed(t, snap, "get_current_time").Counters.Attempts != 1 {
		t.Fatal("the executed call of a cancelled turn must be retained")
	}
	for _, v := range snap.Turns {
		if v.Mode == "proactive" && (v.Turns != 1 || v.RunFailures.Cancelled != 1) {
			t.Fatalf("cancellation must be classified: %+v", v)
		}
	}
}

func TestConversationTurnHallucinatedToolFoldsIntoUnregistered(t *testing.T) {
	p := newProactiveTestBot(t, nil)
	p.llm.responses = []llm.Response{
		toolCallResponse(llm.ToolCall{ID: "1", Name: "definitely_not_a_tool", Arguments: []byte(`{}`)}),
		{Message: llm.TextMessage(llm.RoleAssistant, "ok")},
	}

	toolStatsTurn(context.Background(), t, p, llmcontext.TriggerMention, adminconfig.ResolvedChatConfig{})

	snap := p.bot.stats.ToolSnapshot(statistics.GlobalScope())
	var found bool
	for _, row := range snap.Tools {
		if row.Overflow == "unregistered" {
			found = true
			if row.Counters.Rejected != 1 || row.Rejections.NotRegistered != 1 {
				t.Fatalf("unregistered row %+v", row)
			}
		}
		if row.Tool != nil && *row.Tool == "definitely_not_a_tool" {
			t.Fatal("a hallucinated name must never become a key")
		}
	}
	if !found {
		t.Fatal("expected the unregistered overflow row")
	}
}

func TestConversationTurnToolResetDuringTurnDropsTheTurn(t *testing.T) {
	p := newProactiveTestBot(t, nil)
	p.bot.tools = tooluse.New(p.llm, p.history, nil, nil, apiActionSender{api: p.apiBot}, nil, p.bot.logger, tooluse.Config{
		MaxIterations: 6,
		BotIdentity:   tooluse.BotIdentity{UserID: testBotID.ID, Name: testBotID.FirstName},
		Dispatcher: silenceEventHook(func(event execution.Event) {
			if _, ok := event.(execution.ToolCompleted); ok {
				p.bot.stats.ResetToolUsage() // an admin reset while the turn is in flight
			}
		}),
	})
	p.llm.responses = []llm.Response{
		toolCallResponse(llm.ToolCall{ID: "1", Name: "get_current_time", Arguments: []byte(`{}`)}),
		{Message: llm.TextMessage(llm.RoleAssistant, "done")},
	}

	toolStatsTurn(context.Background(), t, p, llmcontext.TriggerMention, adminconfig.ResolvedChatConfig{})

	snap := p.bot.stats.ToolSnapshot(statistics.GlobalScope())
	if len(snap.Tools) != 0 {
		t.Fatalf("a turn that straddles a reset contributes nothing: %+v", snap.Tools)
	}
	for _, v := range snap.Turns {
		if v.Turns != 0 {
			t.Fatalf("turn counters must be empty after the reset: %+v", snap.Turns)
		}
	}
}

func TestToolRunFailureExcludesFinalDelivery(t *testing.T) {
	// The observation point precedes final Telegram delivery, so a delivery
	// failure class never appears at it.
	if got := toolRunFailureOf(nil); got != statistics.ToolRunOK {
		t.Fatalf("no error is OK, got %v", got)
	}
	if got := toolRunFailureOf(context.Canceled); got != statistics.ToolRunCancelled {
		t.Fatalf("got %v", got)
	}
	if got := toolRunFailureOf(errors.Join(ErrRequestTimeout, context.DeadlineExceeded)); got != statistics.ToolRunTimeout {
		t.Fatalf("got %v", got)
	}
	if got := toolRunFailureOf(errors.New("boom")); got != statistics.ToolRunGeneration {
		t.Fatalf("got %v", got)
	}
}

func TestToolCallObservationMapsAuthoritativeRecordFields(t *testing.T) {
	obs := toolCallObservationOf(tooluse.ToolCallRecord{
		Name: "x", Status: execution.StatusError, Executed: false, ErrorCode: "tool_unavailable", ErrorClass: tooluse.ErrorUnavailable,
		Rejection: tooluse.RejectionNotAdvertised, ResultPreparation: tooluse.ResultPreparationReduced,
		SideEffecting: true, SideEffectOutcome: "unknown",
	})
	if obs.Rejection != statistics.ToolRejectedNotAdvertised || obs.Preparation != statistics.ToolPreparationReduced ||
		obs.ErrorClass != "unavailable" || !obs.SideEffecting || obs.SideEffectOutcome != "unknown" || obs.Executed {
		t.Fatalf("%+v", obs)
	}
}
