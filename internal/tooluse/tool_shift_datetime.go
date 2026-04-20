package tooluse

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

func shiftDateTimeHandler(_ context.Context, _ CallContext, args json.RawMessage) (toolResult, error) {
	var payload struct {
		Timestamp string `json:"timestamp"`
		Days      int    `json:"days"`
		Hours     int    `json:"hours"`
		Minutes   int    `json:"minutes"`
	}
	if err := json.Unmarshal(args, &payload); err != nil {
		return toolResult{}, fmt.Errorf("parse arguments: %w", err)
	}
	if payload.Timestamp == "" {
		return toolResult{}, errors.New("timestamp is required")
	}
	if payload.Days == 0 && payload.Hours == 0 && payload.Minutes == 0 {
		return toolResult{}, errors.New("at least one non-zero delta is required")
	}

	sourceTime, err := time.Parse(time.RFC3339, payload.Timestamp)
	if err != nil {
		return toolResult{}, fmt.Errorf("parse timestamp: %w", err)
	}

	shifted := sourceTime.AddDate(0, 0, payload.Days)
	duration := time.Duration(payload.Hours)*time.Hour + time.Duration(payload.Minutes)*time.Minute
	shifted = shifted.Add(duration)
	_, offset := shifted.Zone()

	return toolResult{
		Status:  "ok",
		Summary: "Datetime shifted",
		Data: map[string]any{
			"original_timestamp_rfc3339": sourceTime.Format(time.RFC3339),
			"shifted_timestamp_rfc3339":  shifted.Format(time.RFC3339),
			"timezone":                   shifted.Location().String(),
			"weekday":                    shifted.Weekday().String(),
			"utc_offset_seconds":         offset,
			"applied_delta": map[string]any{
				"days":    payload.Days,
				"hours":   payload.Hours,
				"minutes": payload.Minutes,
			},
		},
	}, nil
}
