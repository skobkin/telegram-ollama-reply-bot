package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"telegram-ollama-reply-bot/internal/reminders"
)

func (s *Store) CreateReminder(ctx context.Context, reminder reminders.Reminder) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO reminders(
  id, chat_id, topic_id, creator_user_id, text, schedule_type, timezone, next_due_at, last_delivered_at,
  interval_value, weekday_mask, day_of_month, time_of_day_minutes, is_active, created_at, updated_at
)
VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
`,
		reminder.ID,
		reminder.Scope.ChatID,
		reminder.Scope.TopicID,
		reminder.CreatorUserID,
		reminder.Text,
		string(reminder.Schedule.Type),
		reminder.Schedule.Timezone,
		formatSQLiteTime(reminder.NextDueAt),
		formatSQLiteTime(reminder.LastDeliveredAt),
		reminder.Schedule.IntervalValue,
		int(reminder.Schedule.WeekdayMask),
		reminder.Schedule.DayOfMonth,
		reminder.Schedule.TimeOfDayMinutes,
		boolToSQLite(reminder.Active),
		formatSQLiteTime(reminder.CreatedAt),
		formatSQLiteTime(reminder.UpdatedAt),
	)
	if err != nil {
		return fmt.Errorf("create reminder: %w", err)
	}

	return nil
}

func (s *Store) ListActiveReminders(ctx context.Context, chatID int64) ([]reminders.Reminder, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id, chat_id, topic_id, creator_user_id, text, schedule_type, timezone, next_due_at, last_delivered_at,
       interval_value, weekday_mask, day_of_month, time_of_day_minutes, is_active, created_at, updated_at
FROM reminders
WHERE chat_id = ? AND is_active = 1
ORDER BY next_due_at ASC, id ASC
`, chatID)
	if err != nil {
		return nil, fmt.Errorf("list active reminders: %w", err)
	}
	defer func() { _ = rows.Close() }()

	return scanReminders(rows)
}

func (s *Store) LoadActiveReminders(ctx context.Context) ([]reminders.Reminder, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id, chat_id, topic_id, creator_user_id, text, schedule_type, timezone, next_due_at, last_delivered_at,
       interval_value, weekday_mask, day_of_month, time_of_day_minutes, is_active, created_at, updated_at
FROM reminders
WHERE is_active = 1
ORDER BY next_due_at ASC, id ASC
`)
	if err != nil {
		return nil, fmt.Errorf("load active reminders: %w", err)
	}
	defer func() { _ = rows.Close() }()

	return scanReminders(rows)
}

func (s *Store) GetReminder(ctx context.Context, reminderID string) (reminders.Reminder, bool, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id, chat_id, topic_id, creator_user_id, text, schedule_type, timezone, next_due_at, last_delivered_at,
       interval_value, weekday_mask, day_of_month, time_of_day_minutes, is_active, created_at, updated_at
FROM reminders
WHERE id = ?
`, reminderID)
	if err != nil {
		return reminders.Reminder{}, false, fmt.Errorf("get reminder: %w", err)
	}
	defer func() { _ = rows.Close() }()

	items, err := scanReminders(rows)
	if err != nil {
		return reminders.Reminder{}, false, err
	}
	if len(items) == 0 {
		return reminders.Reminder{}, false, nil
	}

	return items[0], true, nil
}

func (s *Store) UpdateReminderDelivery(ctx context.Context, reminderID string, deliveredAt time.Time, nextDueAt time.Time, active bool) error {
	_, err := s.db.ExecContext(ctx, `
UPDATE reminders
SET last_delivered_at = ?, next_due_at = ?, is_active = ?, updated_at = CURRENT_TIMESTAMP
WHERE id = ?
`, formatSQLiteTime(deliveredAt), formatSQLiteTime(nextDueAt), boolToSQLite(active), reminderID)
	if err != nil {
		return fmt.Errorf("update reminder delivery: %w", err)
	}

	return nil
}

func (s *Store) CancelReminder(ctx context.Context, reminderID string) error {
	_, err := s.db.ExecContext(ctx, `
UPDATE reminders
SET is_active = 0, updated_at = CURRENT_TIMESTAMP
WHERE id = ?
`, reminderID)
	if err != nil {
		return fmt.Errorf("cancel reminder: %w", err)
	}

	return nil
}

func scanReminders(rows *sql.Rows) ([]reminders.Reminder, error) {
	result := make([]reminders.Reminder, 0)
	for rows.Next() {
		var reminder reminders.Reminder
		var nextDueAt string
		var lastDeliveredAt string
		var createdAt string
		var updatedAt string
		var active int
		var weekdayMask int

		err := rows.Scan(
			&reminder.ID,
			&reminder.Scope.ChatID,
			&reminder.Scope.TopicID,
			&reminder.CreatorUserID,
			&reminder.Text,
			&reminder.Schedule.Type,
			&reminder.Schedule.Timezone,
			&nextDueAt,
			&lastDeliveredAt,
			&reminder.Schedule.IntervalValue,
			&weekdayMask,
			&reminder.Schedule.DayOfMonth,
			&reminder.Schedule.TimeOfDayMinutes,
			&active,
			&createdAt,
			&updatedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("scan reminder: %w", err)
		}

		if weekdayMask < 0 || weekdayMask > 127 {
			return nil, fmt.Errorf("invalid weekday mask %d", weekdayMask)
		}
		reminder.Schedule.WeekdayMask = reminders.WeekdayMask(uint8(weekdayMask))
		reminder.NextDueAt = parseSQLiteTime(nextDueAt)
		reminder.LastDeliveredAt = parseSQLiteTime(lastDeliveredAt)
		reminder.Active = active == 1
		reminder.CreatedAt = parseSQLiteTime(createdAt)
		reminder.UpdatedAt = parseSQLiteTime(updatedAt)
		result = append(result, reminder)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate reminders: %w", err)
	}

	return result, nil
}
