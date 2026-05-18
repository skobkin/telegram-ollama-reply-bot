package tooluse

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"time"

	"telegram-ollama-reply-bot/internal/state"
)

const (
	chatActivityRecent10m = 10 * time.Minute
	chatActivityRecent1h  = time.Hour
)

func (r *Runtime) getChatActivityWindow(_ context.Context, callCtx CallContext, _ json.RawMessage) (toolResult, error) {
	snapshot := r.history.Snapshot(callCtx.Scope)
	if len(snapshot.Messages) == 0 {
		result := toolResult{
			Status:  "empty",
			Summary: "No recent in-memory messages are available for the current chat/topic",
			Data: map[string]any{
				"chat_id":  callCtx.Scope.ChatID,
				"topic_id": callCtx.Scope.TopicID,
			},
		}
		logToolResult(callCtx, result, "data", result.Data)

		return result, nil
	}

	first := snapshot.Messages[0].CreatedAt.UTC()
	last := snapshot.Messages[len(snapshot.Messages)-1].CreatedAt.UTC()
	now := time.Now().UTC()
	if now.Before(last) {
		now = last
	}

	data := map[string]any{
		"chat_id":                    callCtx.Scope.ChatID,
		"topic_id":                   callCtx.Scope.TopicID,
		"message_count":              len(snapshot.Messages),
		"first_message_at":           first.Format(time.RFC3339),
		"last_message_at":            last.Format(time.RFC3339),
		"seconds_since_last_message": int64(now.Sub(last).Seconds()),
		"window_span_seconds":        int64(last.Sub(first).Seconds()),
		"messages_in_last_10m":       countMessagesSince(snapshot.Messages, last.Add(-chatActivityRecent10m)),
		"messages_in_last_1h":        countMessagesSince(snapshot.Messages, last.Add(-chatActivityRecent1h)),
	}

	averageGapSeconds, burstiness := activityGapStats(snapshot.Messages)
	data["average_gap_seconds"] = averageGapSeconds
	data["burstiness"] = burstiness

	result := toolResult{
		Status:  "ok",
		Summary: buildActivitySummary(len(snapshot.Messages), first, last, burstiness),
		Data:    data,
	}
	logToolResult(callCtx, result, "data", data)

	return result, nil
}

func countMessagesSince(messages []state.Message, threshold time.Time) int {
	count := 0
	for _, msg := range messages {
		if !msg.CreatedAt.Before(threshold) {
			count++
		}
	}

	return count
}

func activityGapStats(messages []state.Message) (float64, string) {
	if len(messages) < 2 {
		return 0, "insufficient_data"
	}

	gaps := make([]float64, 0, len(messages)-1)
	var total float64
	for i := 1; i < len(messages); i++ {
		gap := messages[i].CreatedAt.Sub(messages[i-1].CreatedAt).Seconds()
		if gap < 0 {
			gap = 0
		}
		gaps = append(gaps, gap)
		total += gap
	}

	average := total / float64(len(gaps))
	if len(messages) < 3 || average == 0 {
		return average, "insufficient_data"
	}

	var variance float64
	for _, gap := range gaps {
		diff := gap - average
		variance += diff * diff
	}

	cv := math.Sqrt(variance/float64(len(gaps))) / average
	switch {
	case cv < 0.35:
		return average, "steady"
	case cv < 1:
		return average, "mixed"
	default:
		return average, "bursty"
	}
}

func buildActivitySummary(messageCount int, first, last time.Time, burstiness string) string {
	return fmt.Sprintf(
		"Analyzed %s recent message(s) from %s to %s with burstiness=%s",
		jsonInt(messageCount),
		first.Format(time.RFC3339),
		last.Format(time.RFC3339),
		burstiness,
	)
}

func jsonInt(v int) string {
	return strconv.Itoa(v)
}
