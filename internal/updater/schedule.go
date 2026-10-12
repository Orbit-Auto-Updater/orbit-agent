package updater

import (
	"time"

	"linux-dash-agent/internal/config"
	"linux-dash-agent/internal/protocol"
)

const defaultWindowMinutes = 120

// Due reports whether a scheduled run should start at now. For window
// policies it returns the start of the current window, which the caller
// records so each window runs at most once. lastWindow is the last window
// already handled; lastRun is when the last scheduled run started.
func Due(p protocol.Policy, now, lastWindow, lastRun time.Time) (windowStart time.Time, due bool) {
	if !p.Enabled {
		return time.Time{}, false
	}
	if w := p.Window; w != nil {
		loc, err := time.LoadLocation(w.Timezone)
		if err != nil {
			return time.Time{}, false
		}
		start, err := time.Parse("15:04", w.Start)
		if err != nil {
			return time.Time{}, false
		}
		dur := time.Duration(w.DurationMinutes) * time.Minute
		if dur == 0 {
			dur = defaultWindowMinutes * time.Minute
		}
		local := now.In(loc)
		// The window may have opened yesterday and run past midnight.
		for _, offset := range []int{0, -1} {
			d := local.AddDate(0, 0, offset)
			s := time.Date(d.Year(), d.Month(), d.Day(), start.Hour(), start.Minute(), 0, 0, loc)
			if !dayAllowed(w.Days, s.Weekday()) {
				continue
			}
			if !local.Before(s) && local.Before(s.Add(dur)) && !s.Equal(lastWindow) && s.After(lastWindow) {
				return s, true
			}
		}
		return time.Time{}, false
	}
	if p.IntervalHours > 0 {
		if lastRun.IsZero() || now.Sub(lastRun) >= time.Duration(p.IntervalHours)*time.Hour {
			return now, true
		}
	}
	return time.Time{}, false
}

func dayAllowed(days []string, wd time.Weekday) bool {
	if len(days) == 0 {
		return true
	}
	for _, d := range days {
		if w, ok := config.ParseWeekday(d); ok && w == wd {
			return true
		}
	}
	return false
}
