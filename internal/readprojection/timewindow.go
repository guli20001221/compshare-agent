package readprojection

import (
	"strings"
	"time"
)

var monitorNowFunc = time.Now

var monitorHistoryLoc = func() *time.Location {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		return time.FixedZone("CST", 8*60*60)
	}
	return loc
}()

// ResolveMonitorHistoryWindow converts the typed tool contract into timestamps.
// The model selects semantics; the server owns all calendar arithmetic.
func ResolveMonitorHistoryWindow(window *TimeWindow) (int64, int64, bool) {
	if window == nil {
		return 0, 0, false
	}
	loc, ok := monitorLocation(window.Timezone)
	if !ok {
		return 0, 0, false
	}
	now := monitorNowFunc().In(loc)
	var start, end time.Time
	switch window.Type {
	case TimeWindowPreset:
		switch window.Preset {
		case "yesterday":
			start = startOfDayIn(now, loc).AddDate(0, 0, -1)
			end = start.AddDate(0, 0, 1)
		case "today":
			start, end = startOfDayIn(now, loc), now
		default:
			return 0, 0, false
		}
	case TimeWindowRelative:
		if window.Amount <= 0 {
			return 0, 0, false
		}
		var duration time.Duration
		switch window.Unit {
		case "minute":
			duration = time.Duration(window.Amount) * time.Minute
		case "hour":
			duration = time.Duration(window.Amount) * time.Hour
		case "day":
			duration = time.Duration(window.Amount) * 24 * time.Hour
		default:
			return 0, 0, false
		}
		start, end = now.Add(-duration), now
	case TimeWindowAbsolute:
		var startOK, endOK bool
		start, startOK = parseMonitorTimestamp(window.Start, loc)
		end, endOK = parseMonitorTimestamp(window.End, loc)
		if !startOK || !endOK {
			return 0, 0, false
		}
	default:
		return 0, 0, false
	}
	if !end.After(start) || end.Sub(start) > 30*24*time.Hour {
		return 0, 0, false
	}
	return start.Unix(), end.Unix(), true
}

func monitorLocation(name string) (*time.Location, bool) {
	switch strings.TrimSpace(name) {
	case "", "Asia/Shanghai":
		return monitorHistoryLoc, true
	case "UTC":
		return time.UTC, true
	default:
		return nil, false
	}
}

func parseMonitorTimestamp(value string, loc *time.Location) (time.Time, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, false
	}
	if parsed, err := time.Parse(time.RFC3339, value); err == nil {
		return parsed, true
	}
	for _, layout := range []string{"2006-01-02 15:04:05", "2006-01-02 15:04"} {
		if parsed, err := time.ParseInLocation(layout, value, loc); err == nil {
			return parsed, true
		}
	}
	return time.Time{}, false
}

func startOfDayIn(value time.Time, loc *time.Location) time.Time {
	year, month, day := value.In(loc).Date()
	return time.Date(year, month, day, 0, 0, 0, 0, loc)
}
