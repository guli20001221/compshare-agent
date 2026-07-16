package readprojection

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

func IsCurrentMonitorTimeWindow(window *TimeWindow) bool {
	if window == nil {
		return true
	}
	if window.Type != TimeWindowPreset {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(window.Value)) {
	case "now", "current", "realtime":
		return true
	default:
		return false
	}
}

var monitorNowFunc = time.Now

var monitorHistoryLoc = func() *time.Location {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		return time.FixedZone("CST", 8*3600)
	}
	return loc
}()

var relativeMonitorWindowRE = regexp.MustCompile(`(?i)(?:过去|最近|近|past|last|previous)\s*(\d+)\s*([a-z]+|分钟|分|小时|时)`)

func ResolveMonitorHistoryWindow(window *TimeWindow) (int64, int64, bool) {
	if window == nil {
		return 0, 0, false
	}
	now := monitorNowFunc().In(monitorHistoryLoc)
	var start, end time.Time
	switch window.Type {
	case TimeWindowAbsolute:
		s, e, ok := parseAbsoluteMonitorWindow(window.Value)
		if !ok {
			return 0, 0, false
		}
		start, end = s, e
	case TimeWindowPreset:
		switch strings.ToLower(strings.TrimSpace(window.Value)) {
		case "yesterday", "昨天":
			day := startOfDay(now).AddDate(0, 0, -1)
			start, end = day, day.Add(24*time.Hour)
		case "today", "今天":
			start, end = startOfDay(now), now
		default:
			return 0, 0, false
		}
	case TimeWindowRelative:
		s, e, ok := parseRelativeMonitorWindow(window.Value, now)
		if !ok {
			return 0, 0, false
		}
		start, end = s, e
	default:
		return 0, 0, false
	}
	if !end.After(start) || end.Sub(start) > 24*time.Hour {
		return 0, 0, false
	}
	return start.Unix(), end.Unix(), true
}

func atoiDefault(value string, fallback int) int {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	n, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return n
}

func validClock(hour, minute int) bool {
	return hour >= 0 && hour <= 23 && minute >= 0 && minute <= 59
}

func parseAbsoluteMonitorWindow(value string) (time.Time, time.Time, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, time.Time{}, false
	}
	for _, sep := range []string{"/", "~", "～", "到", "至"} {
		parts := strings.Split(value, sep)
		if len(parts) != 2 {
			continue
		}
		start, okStart := parseMonitorTime(parts[0])
		end, okEnd := parseMonitorTimeWithDefaultDate(parts[1], start)
		if okStart && okEnd {
			return start, end, true
		}
	}
	return time.Time{}, time.Time{}, false
}

func parseRelativeMonitorWindow(value string, now time.Time) (time.Time, time.Time, bool) {
	lower := strings.ToLower(strings.TrimSpace(value))
	if lower == "" {
		return time.Time{}, time.Time{}, false
	}
	if strings.Contains(lower, "yesterday") || strings.Contains(lower, "昨天") {
		day := startOfDay(now).AddDate(0, 0, -1)
		return day, day.Add(24 * time.Hour), true
	}
	m := relativeMonitorWindowRE.FindStringSubmatch(lower)
	if len(m) != 3 {
		return time.Time{}, time.Time{}, false
	}
	n, err := strconv.Atoi(m[1])
	if err != nil || n <= 0 {
		return time.Time{}, time.Time{}, false
	}
	unit := strings.ToLower(strings.TrimSpace(m[2]))
	d := time.Duration(n) * time.Minute
	switch unit {
	case "分钟", "分", "minute", "minutes", "min", "m":
		d = time.Duration(n) * time.Minute
	case "小时", "时", "hour", "hours", "h":
		d = time.Duration(n) * time.Hour
	default:
		return time.Time{}, time.Time{}, false
	}
	return now.Add(-d), now, true
}

func parseMonitorTime(value string) (time.Time, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, false
	}
	if match := regexp.MustCompile(`(\d{4}-\d{2}-\d{2})\s+(\d{1,2}):(\d{1,2})(?::(\d{1,2}))?`).FindStringSubmatch(value); len(match) > 0 {
		second := match[4]
		if second == "" {
			second = "00"
		}
		normalized := fmt.Sprintf("%s %02s:%02s:%02s", match[1], match[2], match[3], second)
		if t, err := time.ParseInLocation("2006-01-02 15:04:05", normalized, monitorHistoryLoc); err == nil {
			return t, true
		}
	}
	if match := regexp.MustCompile(`(\d{4}-\d{2}-\d{2})T(\d{1,2}:\d{2}(?::\d{2})?(?:Z|[+-]\d{2}:\d{2}))`).FindStringSubmatch(value); len(match) > 0 {
		if t, err := time.Parse(time.RFC3339, match[1]+"T"+match[2]); err == nil {
			return t, true
		}
	}
	if t, err := time.Parse(time.RFC3339, value); err == nil {
		return t, true
	}
	for _, layout := range []string{"2006-01-02 15:04:05", "2006-01-02 15:04"} {
		if t, err := time.ParseInLocation(layout, value, monitorHistoryLoc); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

func parseMonitorTimeWithDefaultDate(value string, defaultDate time.Time) (time.Time, bool) {
	if t, ok := parseMonitorTime(value); ok {
		return t, true
	}
	value = strings.TrimSpace(value)
	m := regexp.MustCompile(`(?:^|\D)(\d{1,2})(?:\s*(?::|点|时)\s*(\d{1,2})?)?`).FindStringSubmatch(value)
	if len(m) == 0 || defaultDate.IsZero() {
		return time.Time{}, false
	}
	hour, err := strconv.Atoi(m[1])
	if err != nil {
		return time.Time{}, false
	}
	minute := atoiDefault(m[2], 0)
	if !validClock(hour, minute) {
		return time.Time{}, false
	}
	base := defaultDate.In(monitorHistoryLoc)
	y, mon, d := base.Date()
	return time.Date(y, mon, d, hour, minute, 0, 0, monitorHistoryLoc), true
}

func startOfDay(t time.Time) time.Time {
	y, m, d := t.In(monitorHistoryLoc).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, monitorHistoryLoc)
}
