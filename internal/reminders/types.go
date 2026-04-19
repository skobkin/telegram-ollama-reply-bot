package reminders

import (
	"context"
	"fmt"
	"strings"
	"time"

	"telegram-ollama-reply-bot/internal/state"
)

type ScheduleType string

const (
	ScheduleTypeOneShot       ScheduleType = "one_shot"
	ScheduleTypeIntervalDays  ScheduleType = "interval_days"
	ScheduleTypeIntervalWeeks ScheduleType = "interval_weeks"
	ScheduleTypeWeekdays      ScheduleType = "weekdays"
	ScheduleTypeMonthly       ScheduleType = "monthly"
)

type WeekdayMask uint8

type Schedule struct {
	Type             ScheduleType
	Timezone         string
	IntervalValue    int
	WeekdayMask      WeekdayMask
	DayOfMonth       int
	TimeOfDayMinutes int
}

type Reminder struct {
	ID              string
	Scope           state.ConversationScope
	CreatorUserID   int64
	Text            string
	Schedule        Schedule
	NextDueAt       time.Time
	LastDeliveredAt time.Time
	Active          bool
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

type CreateRequest struct {
	Text          string   `json:"text"`
	ScheduleType  string   `json:"schedule_type"`
	RunAt         string   `json:"run_at"`
	Timezone      string   `json:"timezone"`
	IntervalValue int      `json:"interval_value"`
	Weekdays      []string `json:"weekdays"`
	DayOfMonth    int      `json:"day_of_month"`
	TimeOfDay     string   `json:"time_of_day"`
}

type Store interface {
	CreateReminder(ctx context.Context, reminder Reminder) error
	ListActiveReminders(ctx context.Context, chatID int64) ([]Reminder, error)
	GetReminder(ctx context.Context, reminderID string) (Reminder, bool, error)
	LoadActiveReminders(ctx context.Context) ([]Reminder, error)
	UpdateReminderDelivery(ctx context.Context, reminderID string, deliveredAt time.Time, nextDueAt time.Time, active bool) error
	CancelReminder(ctx context.Context, reminderID string) error
}

func (m WeekdayMask) Has(day time.Weekday) bool {
	if day < time.Sunday || day > time.Saturday {
		return false
	}

	return m&(1<<day) != 0
}

func (m *WeekdayMask) Add(day time.Weekday) {
	if day < time.Sunday || day > time.Saturday {
		return
	}

	*m |= 1 << day
}

func ParseWeekdays(values []string) (WeekdayMask, error) {
	var mask WeekdayMask
	for _, value := range values {
		day, err := parseWeekday(value)
		if err != nil {
			return 0, err
		}
		mask.Add(day)
	}
	if mask == 0 {
		return 0, fmt.Errorf("weekdays must not be empty")
	}

	return mask, nil
}

func parseWeekday(value string) (time.Weekday, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "sunday":
		return time.Sunday, nil
	case "monday":
		return time.Monday, nil
	case "tuesday":
		return time.Tuesday, nil
	case "wednesday":
		return time.Wednesday, nil
	case "thursday":
		return time.Thursday, nil
	case "friday":
		return time.Friday, nil
	case "saturday":
		return time.Saturday, nil
	default:
		return 0, fmt.Errorf("unsupported weekday %q", value)
	}
}
