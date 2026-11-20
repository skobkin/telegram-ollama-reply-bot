package tooluse

import (
	"context"
	"encoding/json"
	"errors"
	"image/png"
	"io"
	"log/slog"
	"strings"
	"testing"

	"telegram-ollama-reply-bot/internal/content/chart"
	"telegram-ollama-reply-bot/internal/llm"
	"telegram-ollama-reply-bot/internal/state"
	"telegram-ollama-reply-bot/internal/state/memory"

	tg "github.com/mymmrac/telego"
	"github.com/mymmrac/telego/telegoapi"
)

const chartArgsJSON = `{"chart_type":"bar","title":"Votes","categories":["Pizza","Sushi"],"series":[{"name":"Votes","values":[12,9]}],"orientation":"horizontal","width":800,"height":500}`

func chartRuntime(sender *stubActionSender) *Runtime {
	return &Runtime{actions: sender}
}

func TestGenerateChartSendsDecodablePNGToTopic(t *testing.T) {
	sender := &stubActionSender{message: &tg.Message{MessageID: 77}}
	callCtx := CallContext{Scope: state.ConversationScope{ChatID: 42, TopicID: 9}}

	result, err := chartRuntime(sender).generateChart(context.Background(), callCtx, json.RawMessage(chartArgsJSON))
	if err != nil || result.Status != "ok" {
		t.Fatalf("result = %+v err = %v", result, err)
	}
	if sender.photoCalls != 1 {
		t.Fatalf("photo calls = %d", sender.photoCalls)
	}
	params := sender.lastPhotoParams
	if params.ChatID.ID != 42 || params.MessageThreadID != 9 {
		t.Errorf("target = chat %d topic %d", params.ChatID.ID, params.MessageThreadID)
	}
	img, err := png.Decode(params.Photo.File)
	if err != nil {
		t.Fatalf("uploaded file is not a PNG: %v", err)
	}
	if b := img.Bounds(); b.Dx() != 800 || b.Dy() != 500 {
		t.Errorf("size = %dx%d", b.Dx(), b.Dy())
	}
	data := result.Data.(map[string]any)
	if data["message_id"] != 77 || data["chart_type"] != "bar" || data["series_count"] != 1 || data["point_count"] != 2 || data["topic_id"] != 9 {
		t.Errorf("data = %v", data)
	}
}

func TestGenerateChartNoTopicOmitsThreadID(t *testing.T) {
	sender := &stubActionSender{message: &tg.Message{MessageID: 1}}
	_, err := chartRuntime(sender).generateChart(context.Background(), CallContext{Scope: state.ConversationScope{ChatID: 5}}, json.RawMessage(chartArgsJSON))
	if err != nil || sender.lastPhotoParams.MessageThreadID != 0 {
		t.Fatalf("err=%v params=%+v", err, sender.lastPhotoParams)
	}
}

func TestGenerateChartRejectsInvalidInputWithoutSending(t *testing.T) {
	cases := map[string]string{
		"unknown field":   `{"chart_type":"bar","categories":["a"],"series":[{"name":"s","values":[1]}],"extra":1}`,
		"bad json type":   `{"chart_type":"bar","categories":"a","series":[]}`,
		"unknown type":    `{"chart_type":"scatter","categories":["a"],"series":[{"name":"s","values":[1]}]}`,
		"length mismatch": `{"chart_type":"line","categories":["a","b"],"series":[{"name":"s","values":[1]}]}`,
		"pie negative":    `{"chart_type":"pie","categories":["a","b"],"series":[{"name":"s","values":[1,-1]}]}`,
		"huge number":     `{"chart_type":"line","categories":["a"],"series":[{"name":"s","values":[1e999]}]}`,
		"too large":       `{"chart_type":"line","categories":["a"],"series":[{"name":"s","values":[1e16]}]}`,
		"orient on line":  `{"chart_type":"line","orientation":"vertical","categories":["a"],"series":[{"name":"s","values":[1]}]}`,
		"narrow":          `{"chart_type":"line","width":100,"categories":["a"],"series":[{"name":"s","values":[1]}]}`,
		"empty":           `{}`,
	}
	for name, payload := range cases {
		sender := &stubActionSender{message: &tg.Message{MessageID: 1}}
		_, err := chartRuntime(sender).generateChart(context.Background(), CallContext{}, json.RawMessage(payload))
		toolErr := ClassifyError(err, true)
		if err == nil || toolErr.Class != ErrorArguments || !toolErr.Retryable {
			t.Errorf("%s: err=%v classified=%+v", name, err, toolErr)
		}
		if sender.photoCalls != 0 {
			t.Errorf("%s: a photo was sent", name)
		}
	}
}

func TestGenerateChartRenderFailureSendsNothing(t *testing.T) {
	sender := &stubActionSender{message: &tg.Message{MessageID: 1}}
	r := chartRuntime(sender)
	r.chartRenderer = func(chart.Spec) ([]byte, error) { return nil, chart.ErrRender }

	result, err := r.generateChart(context.Background(), CallContext{}, json.RawMessage(chartArgsJSON))
	if err != nil || result.Error == nil || result.Error.Code != "chart_render_failed" || result.Error.Class != ErrorPermanent || sender.photoCalls != 0 {
		t.Fatalf("result=%+v err=%v sends=%d", result.Error, err, sender.photoCalls)
	}
}

func TestGenerateChartCancelledBeforeRenderStartsNothing(t *testing.T) {
	sender := &stubActionSender{message: &tg.Message{MessageID: 1}}
	r := chartRuntime(sender)
	rendered := 0
	r.chartRenderer = func(spec chart.Spec) ([]byte, error) {
		rendered++

		return chart.Render(spec)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	result, err := r.generateChart(ctx, CallContext{}, json.RawMessage(chartArgsJSON))
	if err != nil || result.Error == nil || result.Error.Code != "cancelled" || result.Error.Class != ErrorCancelled || result.Error.Retryable {
		t.Fatalf("result=%+v err=%v", result.Error, err)
	}
	if rendered != 0 || sender.photoCalls != 0 {
		t.Fatalf("rendered=%d sends=%d", rendered, sender.photoCalls)
	}
}

func TestGenerateChartCancelledBetweenRenderAndSendDoesNotDispatch(t *testing.T) {
	sender := &stubActionSender{message: &tg.Message{MessageID: 1}}
	r := chartRuntime(sender)
	ctx, cancel := context.WithCancel(context.Background())
	r.chartRenderer = func(spec chart.Spec) ([]byte, error) {
		data, err := chart.Render(spec)
		cancel() // the generation turn expires while rendering

		return data, err
	}

	result, err := r.generateChart(ctx, CallContext{}, json.RawMessage(chartArgsJSON))
	if err != nil || result.Error == nil || result.Error.Class != ErrorCancelled {
		t.Fatalf("result=%+v err=%v", result.Error, err)
	}
	if sender.photoCalls != 0 {
		t.Fatal("an expired turn must not dispatch the photo")
	}
}

func TestGenerateChartSendFailureClassification(t *testing.T) {
	cases := map[string]struct {
		err       error
		wantCode  string
		wantClass string
	}{
		"transport failure is ambiguous": {errors.New("connection reset"), "outcome_unknown", ErrorAmbiguous},
		"cancellation after dispatch":    {context.Canceled, "outcome_unknown", ErrorAmbiguous},
		"deadline after dispatch":        {context.DeadlineExceeded, "outcome_unknown", ErrorAmbiguous},
		"definitive 400":                 {&telegoapi.Error{ErrorCode: 400, Description: "bad"}, "telegram_rejected", ErrorPermanent},
		"rate limited":                   {&telegoapi.Error{ErrorCode: 429, Parameters: &telegoapi.ResponseParameters{RetryAfter: 7}}, "rate_limited", ErrorRateLimited},
	}
	for name, tc := range cases {
		sender := &stubActionSender{err: tc.err}
		_, err := chartRuntime(sender).generateChart(context.Background(), CallContext{}, json.RawMessage(chartArgsJSON))
		if err == nil {
			t.Fatalf("%s: expected an error", name)
		}
		classified := ClassifyError(err, true)
		if classified.Code != tc.wantCode || classified.Class != tc.wantClass {
			t.Errorf("%s: %+v", name, classified)
		}
		if strings.Contains(classified.Message, "connection reset") {
			t.Errorf("%s: raw error text leaked", name)
		}
	}
}

// The full runtime path: the call is side-effecting, and the authoritative
// record carries the confirmed outcome.
func TestReplyWithToolsRecordsConfirmedChartSideEffect(t *testing.T) {
	stub := &stubLLM{responses: []llm.Response{
		{Message: llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{
			{ID: "call-1", Name: "generate_chart", Arguments: json.RawMessage(chartArgsJSON)},
		}}},
		{Message: llm.Message{Role: llm.RoleAssistant}},
	}}
	sender := &stubActionSender{message: &tg.Message{MessageID: 55}}
	runtime := New(stub, memory.New(memory.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil))).Conversations(),
		&stubExtractor{}, nil, sender, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), Config{MaxIterations: 6})

	result, err := runtime.ReplyWithTools(context.Background(), eventRequest())
	if err != nil {
		t.Fatal(err)
	}
	if len(result.ToolCalls) != 1 {
		t.Fatalf("records = %+v", result.ToolCalls)
	}
	record := result.ToolCalls[0]
	if record.Name != "generate_chart" || !record.SideEffecting || !record.Executed || record.SideEffectOutcome != "confirmed" || record.ResultPreparation != ResultPreparationComplete {
		t.Errorf("record = %+v", record)
	}
	if sender.photoCalls != 1 {
		t.Errorf("photo calls = %d", sender.photoCalls)
	}
}

func TestGenerateChartRegistrationAndSchema(t *testing.T) {
	registry := newRegistry(&Runtime{})
	definition, ok := registry.Lookup("generate_chart")
	if !ok || !definition.SideEffecting || definition.InvocationPolicy != InvocationPolicyDiscretionary {
		t.Fatalf("definition = %+v ok=%v", definition, ok)
	}
	var schema map[string]any
	if err := json.Unmarshal(definition.Parameters, &schema); err != nil {
		t.Fatalf("schema is not valid JSON: %v", err)
	}
	// The schema bounds must match the validator's, so the model is told the
	// same limits that are enforced.
	text := string(definition.Parameters)
	for _, want := range []string{`"maxItems":50`, `"maxItems":8`, `"maxLength":60`, `"minimum":600`, `"maximum":1600`, `"minimum":360`, `"maximum":1200`} {
		if !strings.Contains(text, want) {
			t.Errorf("schema lacks %s", want)
		}
	}
	if chart.MaxCategories != 50 || chart.MaxSeries != 8 || chart.MaxTitleRunes != 60 || chart.MinWidth != 600 || chart.MaxWidth != 1600 || chart.MinHeight != 360 || chart.MaxHeight != 1200 || chart.MaxLabelRunes != 32 {
		t.Error("chart bounds drifted from the advertised schema")
	}
}
