package tooluse

import (
	"context"
	"encoding/json"
	"math"
	"time"
)

type datetimeMathInput struct {
	Operation      string `json:"operation"`
	Timestamp      string `json:"timestamp"`
	Left           string `json:"left"`
	Right          string `json:"right"`
	TargetTimezone string `json:"target_timezone"`
	Years          *int   `json:"years"`
	Months         *int   `json:"months"`
	Days           *int   `json:"days"`
	Hours          *int   `json:"hours"`
	Minutes        *int   `json:"minutes"`
	Seconds        *int   `json:"seconds"`
}

func datetimeMathHandler(_ context.Context, callCtx CallContext, args json.RawMessage) (toolResult, error) {
	var input datetimeMathInput
	if !decodeToolJSON(args, &input) {
		result := datetimeErrorResult("internal_error", "failed to parse arguments")
		logToolResult(callCtx, result)

		return result, nil
	}

	var result toolResult
	switch input.Operation {
	case "diff":
		result = datetimeMathDiff(input)
	case "shift":
		result = datetimeMathShift(input)
	case "weekday":
		result = datetimeMathWeekday(input)
	case "convert_timezone":
		result = datetimeMathConvertTimezone(input)
	case "":
		result = datetimeErrorResult("missing_required_field", "operation is required")
	default:
		result = datetimeErrorResult("invalid_operation", "operation must be one of diff, shift, weekday, convert_timezone")
	}

	logToolResult(callCtx, result, "data", result.Data)

	return result, nil
}

func datetimeMathDiff(input datetimeMathInput) toolResult {
	left, invalid := parseRequiredRFC3339(input.Left, "left")
	if invalid != nil {
		return *invalid
	}

	right, invalid := parseRequiredRFC3339(input.Right, "right")
	if invalid != nil {
		return *invalid
	}

	duration := right.Sub(left)
	durationSeconds := duration.Seconds()

	sign := 0
	switch {
	case durationSeconds > 0:
		sign = 1
	case durationSeconds < 0:
		sign = -1
	}

	return toolResult{
		Status:  "ok",
		Summary: "Datetime diff computed",
		Data: map[string]any{
			"operation":        "diff",
			"left":             left.Format(time.RFC3339),
			"right":            right.Format(time.RFC3339),
			"duration_seconds": int64(math.Round(durationSeconds)),
			"duration_minutes": durationSeconds / 60,
			"duration_hours":   durationSeconds / 3600,
			"duration_days":    durationSeconds / 86400,
			"sign":             sign,
		},
	}
}

func datetimeMathShift(input datetimeMathInput) toolResult {
	sourceTime, invalid := parseRequiredRFC3339(input.Timestamp, "timestamp")
	if invalid != nil {
		return *invalid
	}

	if !input.hasShiftFields() {
		return datetimeErrorResult("empty_shift", "shift requires at least one shift field")
	}

	shifted := sourceTime.AddDate(input.shiftValue(input.Years), input.shiftValue(input.Months), input.shiftValue(input.Days))
	duration := time.Duration(input.shiftValue(input.Hours))*time.Hour +
		time.Duration(input.shiftValue(input.Minutes))*time.Minute +
		time.Duration(input.shiftValue(input.Seconds))*time.Second
	shifted = shifted.Add(duration)

	return toolResult{
		Status:  "ok",
		Summary: "Datetime shifted",
		Data: map[string]any{
			"operation": "shift",
			"input":     sourceTime.Format(time.RFC3339),
			"result":    shifted.Format(time.RFC3339),
		},
	}
}

func datetimeMathWeekday(input datetimeMathInput) toolResult {
	timestamp, invalid := parseRequiredRFC3339(input.Timestamp, "timestamp")
	if invalid != nil {
		return *invalid
	}

	return toolResult{
		Status:  "ok",
		Summary: "Weekday resolved",
		Data: map[string]any{
			"operation":     "weekday",
			"timestamp":     timestamp.Format(time.RFC3339),
			"weekday":       timestamp.Weekday().String(),
			"weekday_index": isoWeekdayIndex(timestamp.Weekday()),
		},
	}
}

func datetimeMathConvertTimezone(input datetimeMathInput) toolResult {
	timestamp, invalid := parseRequiredRFC3339(input.Timestamp, "timestamp")
	if invalid != nil {
		return *invalid
	}

	location, invalid := loadRequiredTimezone(input.TargetTimezone, "target_timezone")
	if invalid != nil {
		return *invalid
	}

	converted := timestamp.In(location)

	return toolResult{
		Status:  "ok",
		Summary: "Timezone converted",
		Data: map[string]any{
			"operation":       "convert_timezone",
			"input":           timestamp.Format(time.RFC3339),
			"target_timezone": input.TargetTimezone,
			"result":          converted.Format(time.RFC3339),
		},
	}
}

func (input datetimeMathInput) hasShiftFields() bool {
	return input.Years != nil || input.Months != nil || input.Days != nil ||
		input.Hours != nil || input.Minutes != nil || input.Seconds != nil
}

func (input datetimeMathInput) shiftValue(value *int) int {
	if value == nil {
		return 0
	}

	return *value
}
