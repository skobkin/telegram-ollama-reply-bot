package tooluse

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"telegram-ollama-reply-bot/internal/reminders"
)

func (r *Runtime) listChatSchedule(ctx context.Context, callCtx CallContext, _ json.RawMessage) (toolResult, error) {
	if r.reminders == nil {
		return toolResult{}, errors.New("reminder service is unavailable")
	}

	items, err := r.reminders.ListChatReminders(ctx, callCtx.Scope.ChatID)
	if err != nil {
		return toolResult{}, err
	}

	result := make([]map[string]any, 0, len(items))
	for _, item := range items {
		result = append(result, map[string]any{
			"id":              item.ID,
			"text":            item.Text,
			"summary":         reminders.ScheduleSummary(item),
			"next_due_at":     item.NextDueAt.Format(time.RFC3339),
			"topic_id":        item.Scope.TopicID,
			"creator_user_id": item.CreatorUserID,
			"can_manage":      item.CreatorUserID == callCtx.Requester.FromID || r.isAdmin(callCtx.Requester.FromID),
		})
	}

	return toolResult{
		Status:  "ok",
		Summary: fmt.Sprintf("Found %d active reminder(s)", len(result)),
		Data: map[string]any{
			"chat_id":   callCtx.Scope.ChatID,
			"topic_id":  callCtx.Scope.TopicID,
			"reminders": result,
		},
	}, nil
}

func (r *Runtime) addScheduleItem(ctx context.Context, callCtx CallContext, args json.RawMessage) (toolResult, error) {
	if r.reminders == nil {
		return toolResult{}, errors.New("reminder service is unavailable")
	}

	var payload reminders.CreateRequest
	if err := json.Unmarshal(args, &payload); err != nil {
		return toolResult{}, fmt.Errorf("parse arguments: %w", err)
	}

	reminder, err := r.reminders.AddReminder(ctx, callCtx.Scope, callCtx.Requester.FromID, payload)
	if err != nil {
		return toolResult{}, err
	}

	return toolResult{
		Status:  "ok",
		Summary: "Reminder created",
		Data: map[string]any{
			"id":          reminder.ID,
			"text":        reminder.Text,
			"summary":     reminders.ScheduleSummary(reminder),
			"next_due_at": reminder.NextDueAt.Format(time.RFC3339),
			"chat_id":     reminder.Scope.ChatID,
			"topic_id":    reminder.Scope.TopicID,
		},
	}, nil
}

func (r *Runtime) removeScheduleItem(ctx context.Context, callCtx CallContext, args json.RawMessage) (toolResult, error) {
	if r.reminders == nil {
		return toolResult{}, errors.New("reminder service is unavailable")
	}

	var payload struct {
		ReminderID string `json:"reminder_id"`
	}
	if err := json.Unmarshal(args, &payload); err != nil {
		return toolResult{}, fmt.Errorf("parse arguments: %w", err)
	}
	if payload.ReminderID == "" {
		return toolResult{}, errors.New("reminder_id is required")
	}

	reminder, err := r.reminders.RemoveReminder(ctx, callCtx.Requester.FromID, payload.ReminderID)
	if err != nil {
		return toolResult{}, err
	}

	return toolResult{
		Status:  "ok",
		Summary: "Reminder removed",
		Data: map[string]any{
			"id":       reminder.ID,
			"text":     reminder.Text,
			"chat_id":  reminder.Scope.ChatID,
			"topic_id": reminder.Scope.TopicID,
		},
	}, nil
}

func (r *Runtime) isAdmin(userID int64) bool {
	for _, candidate := range r.config.AdminIDs {
		if candidate == userID {
			return true
		}
	}

	return false
}
