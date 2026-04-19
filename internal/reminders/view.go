package reminders

import (
	"fmt"
	"time"
)

func ScheduleSummary(reminder Reminder) string {
	switch reminder.Schedule.Type {
	case ScheduleTypeOneShot:
		return fmt.Sprintf("once at %s", reminder.NextDueAt.Format(time.RFC3339))
	case ScheduleTypeIntervalDays:
		return fmt.Sprintf("every %d day(s), next at %s", reminder.Schedule.IntervalValue, reminder.NextDueAt.Format(time.RFC3339))
	case ScheduleTypeIntervalWeeks:
		return fmt.Sprintf("every %d week(s), next at %s", reminder.Schedule.IntervalValue, reminder.NextDueAt.Format(time.RFC3339))
	case ScheduleTypeWeekdays:
		return fmt.Sprintf("on %s at %s (%s), next at %s", weekdaySummary(reminder.Schedule.WeekdayMask), formatMinutes(reminder.Schedule.TimeOfDayMinutes), reminder.Schedule.Timezone, reminder.NextDueAt.Format(time.RFC3339))
	case ScheduleTypeMonthly:
		return fmt.Sprintf("monthly on day %d at %s (%s), next at %s", reminder.Schedule.DayOfMonth, formatMinutes(reminder.Schedule.TimeOfDayMinutes), reminder.Schedule.Timezone, reminder.NextDueAt.Format(time.RFC3339))
	default:
		return reminder.NextDueAt.Format(time.RFC3339)
	}
}

func weekdaySummary(mask WeekdayMask) string {
	names := make([]string, 0, 7)
	for _, day := range []time.Weekday{time.Monday, time.Tuesday, time.Wednesday, time.Thursday, time.Friday, time.Saturday, time.Sunday} {
		if mask.Has(day) {
			names = append(names, day.String())
		}
	}

	return stringsJoin(names, ", ")
}

func formatMinutes(minutes int) string {
	hours := minutes / 60
	mins := minutes % 60

	return fmt.Sprintf("%02d:%02d", hours, mins)
}

func stringsJoin(parts []string, sep string) string {
	if len(parts) == 0 {
		return ""
	}
	result := parts[0]
	for _, part := range parts[1:] {
		result += sep + part
	}

	return result
}
