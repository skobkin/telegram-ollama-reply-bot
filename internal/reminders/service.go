package reminders

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"telegram-ollama-reply-bot/internal/state"

	t "github.com/mymmrac/telego"
	tu "github.com/mymmrac/telego/telegoutil"
)

var (
	ErrReminderNotFound  = errors.New("reminder not found")
	ErrReminderForbidden = errors.New("reminder access forbidden")
)

type Sender interface {
	SendMessage(ctx context.Context, params *t.SendMessageParams) (*t.Message, error)
}

type Service struct {
	store    Store
	sender   Sender
	logger   *slog.Logger
	adminIDs []int64
	now      func() time.Time

	mu        sync.Mutex
	reminders map[string]Reminder
	wake      chan struct{}
}

func NewService(store Store, sender Sender, adminIDs []int64, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}

	return &Service{
		store:     store,
		sender:    sender,
		logger:    logger,
		adminIDs:  append([]int64(nil), adminIDs...),
		now:       func() time.Time { return time.Now().UTC() },
		reminders: make(map[string]Reminder),
		wake:      make(chan struct{}, 1),
	}
}

func (s *Service) Run(ctx context.Context) error {
	loaded, err := s.store.LoadActiveReminders(ctx)
	if err != nil {
		return fmt.Errorf("load active reminders: %w", err)
	}

	s.mu.Lock()
	for _, reminder := range loaded {
		s.reminders[reminder.ID] = reminder
	}
	s.mu.Unlock()
	s.signalWake()

	for {
		wait := s.nextWait()
		if wait < 0 {
			select {
			case <-ctx.Done():
				return nil
			case <-s.wake:
			}

			continue
		}

		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}

			return nil
		case <-s.wake:
			if !timer.Stop() {
				<-timer.C
			}
		case <-timer.C:
		}

		s.processDue(ctx)
	}
}

func (s *Service) ListChatReminders(ctx context.Context, chatID int64) ([]Reminder, error) {
	result, err := s.store.ListActiveReminders(ctx, chatID)
	if err != nil {
		return nil, err
	}

	slices.SortFunc(result, func(a, b Reminder) int {
		if cmp := a.NextDueAt.Compare(b.NextDueAt); cmp != 0 {
			return cmp
		}

		return strings.Compare(a.ID, b.ID)
	})

	return result, nil
}

func (s *Service) AddReminder(ctx context.Context, scope state.ConversationScope, creatorUserID int64, req CreateRequest) (Reminder, error) {
	reminder, err := s.buildReminder(scope, creatorUserID, req)
	if err != nil {
		return Reminder{}, err
	}

	if err := s.store.CreateReminder(ctx, reminder); err != nil {
		return Reminder{}, err
	}

	s.mu.Lock()
	s.reminders[reminder.ID] = reminder
	s.mu.Unlock()
	s.signalWake()

	return reminder, nil
}

func (s *Service) RemoveReminder(ctx context.Context, requesterUserID int64, reminderID string) (Reminder, error) {
	reminder, ok, err := s.store.GetReminder(ctx, reminderID)
	if err != nil {
		return Reminder{}, err
	}
	if !ok || !reminder.Active {
		return Reminder{}, ErrReminderNotFound
	}
	if reminder.CreatorUserID != requesterUserID && !s.isAdmin(requesterUserID) {
		return Reminder{}, ErrReminderForbidden
	}
	if err := s.store.CancelReminder(ctx, reminderID); err != nil {
		return Reminder{}, err
	}

	s.mu.Lock()
	delete(s.reminders, reminderID)
	s.mu.Unlock()
	s.signalWake()

	reminder.Active = false

	return reminder, nil
}

func (s *Service) nextWait() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	wait := time.Duration(-1)
	for _, reminder := range s.reminders {
		if !reminder.Active {
			continue
		}
		delta := reminder.NextDueAt.Sub(now)
		if delta < 0 {
			delta = 0
		}
		if wait < 0 || delta < wait {
			wait = delta
		}
	}

	return wait
}

func (s *Service) processDue(ctx context.Context) {
	now := s.now()

	s.mu.Lock()
	due := make([]Reminder, 0)
	for _, reminder := range s.reminders {
		if reminder.Active && !reminder.NextDueAt.After(now) {
			due = append(due, reminder)
		}
	}
	s.mu.Unlock()

	slices.SortFunc(due, func(a, b Reminder) int {
		if cmp := a.NextDueAt.Compare(b.NextDueAt); cmp != 0 {
			return cmp
		}

		return strings.Compare(a.ID, b.ID)
	})

	for _, reminder := range due {
		if err := s.deliverReminder(ctx, reminder); err != nil {
			s.logger.Error("failed to deliver reminder", "reminder_id", reminder.ID, "chat_id", reminder.Scope.ChatID, "topic_id", reminder.Scope.TopicID, "error", err)
		}
	}
}

func (s *Service) deliverReminder(ctx context.Context, reminder Reminder) error {
	_, nextDue, active, err := computeDeliveryWindow(reminder, s.now())
	if err != nil {
		return err
	}

	params := &t.SendMessageParams{
		ChatID: tu.ID(reminder.Scope.ChatID),
		Text:   "Reminder: " + reminder.Text,
	}
	if reminder.Scope.TopicID != 0 {
		params.MessageThreadID = reminder.Scope.TopicID
	}
	if _, err := s.sender.SendMessage(ctx, params); err != nil {
		return err
	}

	deliveredAt := s.now()
	if err := s.store.UpdateReminderDelivery(ctx, reminder.ID, deliveredAt, nextDue, active); err != nil {
		return err
	}

	s.mu.Lock()
	if !active {
		delete(s.reminders, reminder.ID)
	} else {
		reminder.LastDeliveredAt = deliveredAt
		reminder.NextDueAt = nextDue
		reminder.Active = true
		reminder.UpdatedAt = deliveredAt
		s.reminders[reminder.ID] = reminder
	}
	s.mu.Unlock()
	s.signalWake()

	return nil
}

func computeDeliveryWindow(reminder Reminder, now time.Time) (time.Time, time.Time, bool, error) {
	if reminder.NextDueAt.After(now) {
		return time.Time{}, time.Time{}, reminder.Active, fmt.Errorf("reminder %s is not due yet", reminder.ID)
	}

	switch reminder.Schedule.Type {
	case ScheduleTypeOneShot:
		return reminder.NextDueAt, time.Time{}, false, nil
	case ScheduleTypeIntervalDays, ScheduleTypeIntervalWeeks:
		occurrence := reminder.NextDueAt
		next := nextIntervalDue(reminder, occurrence)
		for !next.After(now) {
			occurrence = next
			next = nextIntervalDue(reminder, occurrence)
		}

		return occurrence, next, true, nil
	case ScheduleTypeWeekdays, ScheduleTypeMonthly:
		occurrence := reminder.NextDueAt
		next, err := nextCalendarDue(reminder.Schedule, occurrence)
		if err != nil {
			return time.Time{}, time.Time{}, false, err
		}
		for !next.After(now) {
			occurrence = next
			next, err = nextCalendarDue(reminder.Schedule, occurrence)
			if err != nil {
				return time.Time{}, time.Time{}, false, err
			}
		}

		return occurrence, next, true, nil
	default:
		return time.Time{}, time.Time{}, false, fmt.Errorf("unsupported schedule type %q", reminder.Schedule.Type)
	}
}

func nextIntervalDue(reminder Reminder, from time.Time) time.Time {
	switch reminder.Schedule.Type {
	case ScheduleTypeIntervalDays:
		return from.Add(time.Duration(reminder.Schedule.IntervalValue) * 24 * time.Hour)
	case ScheduleTypeIntervalWeeks:
		return from.Add(time.Duration(reminder.Schedule.IntervalValue) * 7 * 24 * time.Hour)
	default:
		return time.Time{}
	}
}

func (s *Service) buildReminder(scope state.ConversationScope, creatorUserID int64, req CreateRequest) (Reminder, error) {
	now := s.now()
	req.Text = strings.TrimSpace(req.Text)
	if req.Text == "" {
		return Reminder{}, errors.New("text is required")
	}

	schedule, nextDueAt, err := buildSchedule(now, req)
	if err != nil {
		return Reminder{}, err
	}

	id, err := newReminderID()
	if err != nil {
		return Reminder{}, err
	}

	return Reminder{
		ID:            id,
		Scope:         scope,
		CreatorUserID: creatorUserID,
		Text:          req.Text,
		Schedule:      schedule,
		NextDueAt:     nextDueAt.UTC(),
		Active:        true,
		CreatedAt:     now,
		UpdatedAt:     now,
	}, nil
}

func buildSchedule(now time.Time, req CreateRequest) (Schedule, time.Time, error) {
	schedule := Schedule{Type: ScheduleType(strings.TrimSpace(req.ScheduleType))}

	switch schedule.Type {
	case ScheduleTypeOneShot:
		runAt, err := time.Parse(time.RFC3339, strings.TrimSpace(req.RunAt))
		if err != nil {
			return Schedule{}, time.Time{}, errors.New("run_at must be RFC3339 with timezone offset")
		}
		if !runAt.After(now) {
			return Schedule{}, time.Time{}, errors.New("run_at must be in the future")
		}

		return schedule, runAt.UTC(), nil
	case ScheduleTypeIntervalDays, ScheduleTypeIntervalWeeks:
		if req.IntervalValue <= 0 {
			return Schedule{}, time.Time{}, errors.New("interval_value must be positive")
		}
		schedule.IntervalValue = req.IntervalValue

		runAt, err := time.Parse(time.RFC3339, strings.TrimSpace(req.RunAt))
		if err != nil {
			return Schedule{}, time.Time{}, errors.New("run_at must be RFC3339 with timezone offset")
		}
		if !runAt.After(now) {
			return Schedule{}, time.Time{}, errors.New("run_at must be in the future")
		}

		return schedule, runAt.UTC(), nil
	case ScheduleTypeWeekdays:
		mask, location, minutes, err := parseCalendarFields(req)
		if err != nil {
			return Schedule{}, time.Time{}, err
		}
		schedule.Timezone = location.String()
		schedule.TimeOfDayMinutes = minutes
		schedule.WeekdayMask = mask

		next, err := firstWeekdayDue(now, location, mask, minutes)
		if err != nil {
			return Schedule{}, time.Time{}, err
		}

		return schedule, next.UTC(), nil
	case ScheduleTypeMonthly:
		if req.DayOfMonth < 1 || req.DayOfMonth > 31 {
			return Schedule{}, time.Time{}, errors.New("day_of_month must be between 1 and 31")
		}
		_, location, minutes, err := parseCalendarFields(req)
		if err != nil {
			return Schedule{}, time.Time{}, err
		}
		schedule.Timezone = location.String()
		schedule.TimeOfDayMinutes = minutes
		schedule.DayOfMonth = req.DayOfMonth

		next, err := firstMonthlyDue(now, location, req.DayOfMonth, minutes)
		if err != nil {
			return Schedule{}, time.Time{}, err
		}

		return schedule, next.UTC(), nil
	default:
		return Schedule{}, time.Time{}, fmt.Errorf("unsupported schedule_type %q", req.ScheduleType)
	}
}

func parseCalendarFields(req CreateRequest) (WeekdayMask, *time.Location, int, error) {
	if strings.TrimSpace(req.Timezone) == "" {
		return 0, nil, 0, errors.New("timezone is required for calendar schedules")
	}
	location, err := time.LoadLocation(strings.TrimSpace(req.Timezone))
	if err != nil {
		return 0, nil, 0, fmt.Errorf("load timezone: %w", err)
	}
	minutes, err := parseTimeOfDay(strings.TrimSpace(req.TimeOfDay))
	if err != nil {
		return 0, nil, 0, err
	}
	mask, err := ParseWeekdays(req.Weekdays)
	if err != nil && ScheduleType(strings.TrimSpace(req.ScheduleType)) == ScheduleTypeWeekdays {
		return 0, nil, 0, err
	}

	return mask, location, minutes, nil
}

func parseTimeOfDay(raw string) (int, error) {
	parsed, err := time.Parse("15:04", raw)
	if err != nil {
		return 0, errors.New("time_of_day must use HH:MM 24h format")
	}

	return parsed.Hour()*60 + parsed.Minute(), nil
}

func firstWeekdayDue(now time.Time, location *time.Location, mask WeekdayMask, minutes int) (time.Time, error) {
	base := now.In(location)
	for i := 0; i < 14; i++ {
		day := base.AddDate(0, 0, i)
		if !mask.Has(day.Weekday()) {
			continue
		}
		candidate := time.Date(day.Year(), day.Month(), day.Day(), minutes/60, minutes%60, 0, 0, location)
		if candidate.After(base) {
			return candidate, nil
		}
	}

	return time.Time{}, errors.New("failed to compute next weekday schedule")
}

func firstMonthlyDue(now time.Time, location *time.Location, dayOfMonth, minutes int) (time.Time, error) {
	base := now.In(location)
	for i := 0; i < 24; i++ {
		month := base.AddDate(0, i, 0)
		candidate := monthlyOccurrence(month.Year(), month.Month(), location, dayOfMonth, minutes)
		if candidate.After(base) {
			return candidate, nil
		}
	}

	return time.Time{}, errors.New("failed to compute next monthly schedule")
}

func nextCalendarDue(schedule Schedule, from time.Time) (time.Time, error) {
	location, err := time.LoadLocation(schedule.Timezone)
	if err != nil {
		return time.Time{}, fmt.Errorf("load timezone: %w", err)
	}

	base := from.In(location).Add(time.Minute)
	switch schedule.Type {
	case ScheduleTypeWeekdays:
		return firstWeekdayDue(base, location, schedule.WeekdayMask, schedule.TimeOfDayMinutes)
	case ScheduleTypeMonthly:
		return firstMonthlyDue(base, location, schedule.DayOfMonth, schedule.TimeOfDayMinutes)
	default:
		return time.Time{}, fmt.Errorf("unsupported calendar schedule type %q", schedule.Type)
	}
}

func monthlyOccurrence(year int, month time.Month, location *time.Location, dayOfMonth, minutes int) time.Time {
	lastDay := time.Date(year, month+1, 0, 0, 0, 0, 0, location).Day()
	day := dayOfMonth
	if day > lastDay {
		day = lastDay
	}

	return time.Date(year, month, day, minutes/60, minutes%60, 0, 0, location)
}

func (s *Service) isAdmin(userID int64) bool {
	return slices.Contains(s.adminIDs, userID)
}

func (s *Service) signalWake() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func newReminderID() (string, error) {
	var bytes [8]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", fmt.Errorf("generate reminder id: %w", err)
	}

	return "rem_" + hex.EncodeToString(bytes[:]), nil
}
