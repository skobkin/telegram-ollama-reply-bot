package tooluse

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

func convertTimezoneHandler(_ context.Context, _ CallContext, args json.RawMessage) (toolResult, error) {
	var payload struct {
		Timestamp       string   `json:"timestamp"`
		TargetTimezones []string `json:"target_timezones"`
	}
	if err := json.Unmarshal(args, &payload); err != nil {
		return toolResult{}, fmt.Errorf("parse arguments: %w", err)
	}
	if payload.Timestamp == "" {
		return toolResult{}, errors.New("timestamp is required")
	}
	if len(payload.TargetTimezones) == 0 {
		return toolResult{}, errors.New("target_timezones must contain at least one timezone")
	}
	if len(payload.TargetTimezones) > 4 {
		return toolResult{}, errors.New("target_timezones must contain at most four timezones")
	}

	sourceTime, err := time.Parse(time.RFC3339, payload.Timestamp)
	if err != nil {
		return toolResult{}, fmt.Errorf("parse timestamp: %w", err)
	}

	conversions := make([]map[string]any, 0, len(payload.TargetTimezones))
	for _, timezoneName := range payload.TargetTimezones {
		location, err := time.LoadLocation(timezoneName)
		if err != nil {
			return toolResult{}, fmt.Errorf("load timezone %q: %w", timezoneName, err)
		}

		converted := sourceTime.In(location)
		_, offset := converted.Zone()
		conversions = append(conversions, map[string]any{
			"timezone":           timezoneName,
			"timestamp_rfc3339":  converted.Format(time.RFC3339),
			"wall_clock":         converted.Format("2006-01-02 15:04"),
			"weekday":            converted.Weekday().String(),
			"utc_offset_seconds": offset,
		})
	}

	_, sourceOffset := sourceTime.Zone()

	return toolResult{
		Status:  "ok",
		Summary: fmt.Sprintf("Converted time into %d timezone(s)", len(conversions)),
		Data: map[string]any{
			"source": map[string]any{
				"timestamp_rfc3339":  sourceTime.Format(time.RFC3339),
				"timezone":           sourceTime.Location().String(),
				"wall_clock":         sourceTime.Format("2006-01-02 15:04"),
				"weekday":            sourceTime.Weekday().String(),
				"utc_offset_seconds": sourceOffset,
			},
			"conversions": conversions,
		},
	}, nil
}
