package tooluse

import (
	"context"
	"encoding/json"
	"time"
)

func currentTimeHandler(_ context.Context, _ CallContext, _ json.RawMessage) (toolResult, error) {
	now := time.Now()
	_, offset := now.Zone()

	return toolResult{
		Status:  "ok",
		Summary: "Current time loaded",
		Data: map[string]any{
			"current_time_rfc3339": now.Format(time.RFC3339),
			"current_time_utc":     now.UTC().Format(time.RFC3339),
			"server_timezone":      now.Location().String(),
			"utc_offset_seconds":   offset,
		},
	}, nil
}
