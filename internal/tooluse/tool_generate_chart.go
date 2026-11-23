package tooluse

import (
	"context"
	"encoding/json"
	"errors"

	"telegram-ollama-reply-bot/internal/content/chart"
	"telegram-ollama-reply-bot/internal/telegram/msgdesc"

	tu "github.com/mymmrac/telego/telegoutil"
)

type chartSeriesArgs struct {
	Name   string    `json:"name"`
	Values []float64 `json:"values"`
}

type chartArgs struct {
	ChartType   string            `json:"chart_type"`
	Title       string            `json:"title"`
	Categories  []string          `json:"categories"`
	Series      []chartSeriesArgs `json:"series"`
	Orientation string            `json:"orientation"`
	Stacked     bool              `json:"stacked"`
	XLabel      string            `json:"x_label"`
	YLabel      string            `json:"y_label"`
	Width       int               `json:"width"`
	Height      int               `json:"height"`
}

func (a chartArgs) spec() chart.Spec {
	series := make([]chart.Series, len(a.Series))
	for i, s := range a.Series {
		series[i] = chart.Series{Name: s.Name, Values: s.Values}
	}

	return chart.Spec{
		Type:        chart.Type(a.ChartType),
		Title:       a.Title,
		Categories:  a.Categories,
		Series:      series,
		Orientation: a.Orientation,
		Stacked:     a.Stacked,
		XLabel:      a.XLabel,
		YLabel:      a.YLabel,
		Width:       a.Width,
		Height:      a.Height,
	}
}

// chartCancelledResult reports cancellation observed BEFORE a send was
// dispatched: no side effect was started, so the record says not_applied.
// Cancellation after dispatch is different — the send may have reached
// Telegram — and is classified ambiguous by ClassifyError.
func chartCancelledResult() toolResult {
	return ErrorResult(NewToolError("cancelled", ErrorCancelled, "The chart was cancelled before it was sent.", false, ""))
}

// contextDone reports an expired or cancelled context, including a deadline
// the transport observed before its timer fired.
func contextDone(ctx context.Context) bool {
	return toolContextError(ctx) != nil
}

// tryRender renders the chart; a failure is reported to the model as a
// structured result, so only success is returned here.
func (r *Runtime) tryRender(spec chart.Spec) ([]byte, bool) {
	render := chart.Render
	if r.chartRenderer != nil {
		render = r.chartRenderer
	}
	png, err := render(spec)

	return png, err == nil
}

func (r *Runtime) generateChart(ctx context.Context, callCtx CallContext, args json.RawMessage) (toolResult, error) {
	var payload chartArgs
	if !decodeToolJSON(args, &payload) {
		return toolResult{}, invalidArguments("Provide chart_type, categories and series according to the schema.")
	}
	spec := payload.spec()
	if err := spec.Validate(); err != nil {
		var invalid *chart.ValidationError
		if errors.As(err, &invalid) {
			return toolResult{}, invalidArguments(invalid.Message)
		}

		return toolResult{}, invalidArguments("The chart specification is not valid.")
	}

	if contextDone(ctx) {
		return chartCancelledResult(), nil
	}
	png, rendered := r.tryRender(spec)
	if !rendered {
		result := ErrorResult(NewToolError("chart_render_failed", ErrorPermanent, "The chart could not be rendered.", false, "Simplify the chart data or use another chart type."))
		logToolResult(callCtx, result, "chart_type", payload.ChartType)

		return result, nil
	}
	// An expired generation turn must not start a new side effect.
	if contextDone(ctx) {
		return chartCancelledResult(), nil
	}

	params := tu.Photo(tu.ID(callCtx.Scope.ChatID), tu.FileFromBytes(png, "chart.png"))
	if callCtx.Scope.TopicID != 0 {
		params.MessageThreadID = callCtx.Scope.TopicID
	}
	message, err := r.actions.SendPhoto(ctx, params)
	if err != nil {
		return toolResult{}, err
	}

	messageID := 0
	if message != nil {
		messageID = message.MessageID
	}
	// Described from the spec, not the PNG: no image metadata is stored, so
	// the vision model is never asked to describe the bot's own chart.
	r.recordSent(ctx, callCtx, message, msgdesc.Chart(payload.ChartType, payload.Title))
	points := len(spec.Categories) * len(spec.Series)
	result := toolResult{
		Status:  "ok",
		Summary: "Chart sent",
		Data: map[string]any{
			"chat_id":      callCtx.Scope.ChatID,
			"topic_id":     callCtx.Scope.TopicID,
			"message_id":   messageID,
			"chart_type":   payload.ChartType,
			"series_count": len(spec.Series),
			"point_count":  points,
		},
	}
	logToolResult(callCtx, result, "message_id", messageID, "chart_type", payload.ChartType, "point_count", points)

	return result, nil
}
