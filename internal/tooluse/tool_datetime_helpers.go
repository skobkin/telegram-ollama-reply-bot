package tooluse

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"
)

type toolErrorPayload struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func decodeToolJSON[T any](args json.RawMessage, target *T) bool {
	decoder := json.NewDecoder(bytes.NewReader(args))
	decoder.DisallowUnknownFields()

	return decoder.Decode(target) == nil
}

func datetimeErrorResult(code, message string) toolResult {
	return toolResult{
		Status: "error",
		Data: map[string]any{
			"error": toolErrorPayload{
				Code:    code,
				Message: message,
			},
		},
	}
}

func parseRequiredRFC3339(value, field string) (time.Time, *toolResult) {
	if value == "" {
		result := datetimeErrorResult("missing_required_field", fmt.Sprintf("%s is required", field))

		return time.Time{}, &result
	}

	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		result := datetimeErrorResult("invalid_timestamp", fmt.Sprintf("%s must be a valid RFC3339 timestamp", field))

		return time.Time{}, &result
	}

	return parsed, nil
}

func loadRequiredTimezone(value, field string) (*time.Location, *toolResult) {
	if value == "" {
		result := datetimeErrorResult("missing_required_field", fmt.Sprintf("%s is required", field))

		return nil, &result
	}

	location, err := time.LoadLocation(value)
	if err != nil {
		result := datetimeErrorResult("invalid_timezone", fmt.Sprintf("%s must be a valid IANA timezone", field))

		return nil, &result
	}

	return location, nil
}

func utcOffsetString(value time.Time) string {
	_, offsetSeconds := value.Zone()

	sign := "+"
	if offsetSeconds < 0 {
		sign = "-"
		offsetSeconds = -offsetSeconds
	}

	hours := offsetSeconds / 3600
	minutes := (offsetSeconds % 3600) / 60

	return fmt.Sprintf("%s%02d:%02d", sign, hours, minutes)
}

func timezoneName(value time.Time) string {
	name, _ := value.Zone()
	if name != "" {
		return name
	}

	return value.Location().String()
}

func isoWeekdayIndex(value time.Weekday) int {
	if value == time.Sunday {
		return 7
	}

	return int(value)
}
