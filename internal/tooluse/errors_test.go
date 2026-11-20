package tooluse

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"telegram-ollama-reply-bot/internal/llm"

	"github.com/mymmrac/telego/telegoapi"
)

func TestSafeErrorClassification(t *testing.T) {
	secret := errors.New("password=DO_NOT_EXPOSE")
	cases := []struct {
		err    error
		action bool
		class  string
		retry  bool
	}{
		{invalidArguments("question is required"), true, ErrorArguments, true},
		{secret, true, ErrorAmbiguous, false},
		{secret, false, ErrorPermanent, false},
		{context.Canceled, false, ErrorCancelled, false},
		{context.Canceled, true, ErrorAmbiguous, false},
		{&telegoapi.Error{ErrorCode: 429}, true, ErrorRateLimited, true},
		{&telegoapi.Error{ErrorCode: 403}, true, ErrorPermanent, false},
	}
	for _, c := range cases {
		got := ClassifyError(c.err, c.action)
		if got.Class != c.class || got.Retryable != c.retry || strings.Contains(got.Message, "DO_NOT_EXPOSE") {
			t.Fatalf("%+v", got)
		}
	}
}

func TestRuntimeRecordsFailureAndConfirmedEffectDespiteSerializationFailure(t *testing.T) {
	runtime, model := restrictedRuntime(t, &stubActionSender{})
	calls := 0
	runtime.registry.Register(Definition{Name: "action", SideEffecting: true, Handler: func(context.Context, CallContext, json.RawMessage) (toolResult, error) {
		calls++
		if calls == 1 {
			return toolResult{}, invalidArguments("value is required")
		}

		return toolResult{Status: "ok", Data: make(chan int)}, nil
	}})
	model.responses = []llm.Response{
		{Message: llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "bad", Name: "action"}}}},
		{Message: llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "corrected", Name: "action"}}}},
		{Message: llm.TextMessage(llm.RoleAssistant, "")},
	}
	result, err := runtime.ReplyWithTools(context.Background(), ChatRequest{})
	if err != nil || len(result.ToolCalls) != 2 {
		t.Fatalf("%+v %v", result, err)
	}
	bad, good := result.ToolCalls[0], result.ToolCalls[1]
	if bad.ErrorClass != ErrorArguments || !bad.Retryable || !bad.Executed || good.Status != "ok" || good.SideEffectOutcome != "confirmed" {
		t.Fatal(result.ToolCalls)
	}
	if !strings.Contains(model.lastBuildExtras[len(model.lastBuildExtras)-1].Text(), "result_serialization_failed") {
		t.Fatal("missing delivery error")
	}
}

func TestQuizRequiresExplicitCorrectAnswer(t *testing.T) {
	sender := &stubActionSender{}
	r, _ := restrictedRuntime(t, sender)
	_, err := r.sendQuiz(context.Background(), CallContext{}, json.RawMessage(`{"question":"Choose","options":["A","B"]}`))
	var failure *ToolError
	if !errors.As(err, &failure) || failure.Class != ErrorArguments || !failure.Retryable || sender.lastPollParams != nil {
		t.Fatalf("err=%v params=%+v", err, sender.lastPollParams)
	}
}
