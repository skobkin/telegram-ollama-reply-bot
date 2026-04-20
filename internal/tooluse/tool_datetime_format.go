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

func datetimeFormatHandler(_ context.Context, _ CallContext, args json.RawMessage) (toolResult, error) {
	var input datetimeFormatInput
	if !decodeToolJSON(args, &input) {
		return datetimeErrorResult("internal_error", "failed to parse arguments"), nil
	}

	timestamp, invalid := parseRequiredRFC3339(input.Timestamp, "timestamp")
	if invalid != nil {
		return *invalid, nil
	}

	if input.Style == "" {
		return datetimeErrorResult("missing_required_field", "style is required"), nil
	}

	if _, ok := datetimeFormatLayouts[input.Style]; !ok {
		return datetimeErrorResult("invalid_style", "style must be one of short, long, date_only, time_only, weekday_date"), nil
	}

	if input.TargetTimezone != "" {
		location, invalid := loadRequiredTimezone(input.TargetTimezone, "target_timezone")
		if invalid != nil {
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

	return toolResult{
		Status:  "ok",
		Summary: "Datetime formatted",
		Data:    data,
	}, nil
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
