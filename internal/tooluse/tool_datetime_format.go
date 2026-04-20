package tooluse

import (
	"context"
	"encoding/json"
	"time"
)

type datetimeFormatInput struct {
	Timestamp      string `json:"timestamp"`
	Style          string `json:"style"`
	TargetTimezone string `json:"target_timezone"`
	Locale         string `json:"locale"`
}

func datetimeFormatHandler(_ context.Context, callCtx CallContext, args json.RawMessage) (toolResult, error) {
	var input datetimeFormatInput
	if !decodeToolJSON(args, &input) {
		result := datetimeErrorResult("internal_error", "failed to parse arguments")
		logToolResult(callCtx, result)

		return result, nil
	}

	timestamp, invalid := parseRequiredRFC3339(input.Timestamp, "timestamp")
	if invalid != nil {
		logToolResult(callCtx, *invalid)

		return *invalid, nil
	}

	if input.Style == "" {
		result := datetimeErrorResult("missing_required_field", "style is required")
		logToolResult(callCtx, result)

		return result, nil
	}

	if _, ok := datetimeFormatLayouts[input.Style]; !ok {
		result := datetimeErrorResult("invalid_style", "style must be one of short, long, date_only, time_only, weekday_date")
		logToolResult(callCtx, result)

		return result, nil
	}

	if input.TargetTimezone != "" {
		location, invalid := loadRequiredTimezone(input.TargetTimezone, "target_timezone")
		if invalid != nil {
			logToolResult(callCtx, *invalid)

			return *invalid, nil
		}

		timestamp = timestamp.In(location)
	}

	data := map[string]any{
		"input":      input.Timestamp,
		"style":      input.Style,
		"formatted":  datetimeFormatLayouts[input.Style](timestamp),
		"timezone":   timezoneName(timestamp),
		"utc_offset": utcOffsetString(timestamp),
	}
	if input.TargetTimezone != "" {
		data["target_timezone"] = input.TargetTimezone
	}

	result := toolResult{
		Status:  "ok",
		Summary: "Datetime formatted",
		Data:    data,
	}
	logToolResult(callCtx, result, "data", data)

	return result, nil
}

var datetimeFormatLayouts = map[string]func(time.Time) string{
	"short": func(value time.Time) string {
		return value.Format("2006-01-02 15:04")
	},
	"long": func(value time.Time) string {
		return value.Format("2006-01-02 15:04 MST")
	},
	"date_only": func(value time.Time) string {
		return value.Format("2006-01-02")
	},
	"time_only": func(value time.Time) string {
		return value.Format("15:04")
	},
	"weekday_date": func(value time.Time) string {
		return value.Format("Monday, 2006-01-02")
	},
}
