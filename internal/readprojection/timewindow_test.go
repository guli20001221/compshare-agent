package readprojection

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMonitorHistoryWindowParserAcceptsChineseRangeExamples(t *testing.T) {
	start, end, ok := ResolveMonitorHistoryWindow(&TimeWindow{
		Type:  TimeWindowAbsolute,
		Value: "2026-05-08 01:00 到 02:00",
	})

	require.True(t, ok)
	assert.Equal(t, int64(1778173200), start)
	assert.Equal(t, int64(1778176800), end)
}

func TestMonitorHistoryWindowParserAcceptsRelativeHoursSlot(t *testing.T) {
	orig := monitorNowFunc
	loc := monitorHistoryLoc
	monitorNowFunc = func() time.Time {
		return time.Date(2026, 6, 22, 12, 0, 0, 0, loc)
	}
	t.Cleanup(func() { monitorNowFunc = orig })

	start, end, ok := ResolveMonitorHistoryWindow(&TimeWindow{
		Type:  TimeWindowRelative,
		Value: "过去 3 小时",
	})

	require.True(t, ok)
	assert.Equal(t, time.Date(2026, 6, 22, 9, 0, 0, 0, loc).Unix(), start)
	assert.Equal(t, time.Date(2026, 6, 22, 12, 0, 0, 0, loc).Unix(), end)
}

func TestMonitorHistoryWindowParserAcceptsRelativeMinutesSlot(t *testing.T) {
	orig := monitorNowFunc
	loc := monitorHistoryLoc
	monitorNowFunc = func() time.Time {
		return time.Date(2026, 6, 22, 12, 0, 0, 0, loc)
	}
	t.Cleanup(func() { monitorNowFunc = orig })

	start, end, ok := ResolveMonitorHistoryWindow(&TimeWindow{
		Type:  TimeWindowRelative,
		Value: "最近 30 分钟",
	})

	require.True(t, ok)
	assert.Equal(t, time.Date(2026, 6, 22, 11, 30, 0, 0, loc).Unix(), start)
	assert.Equal(t, time.Date(2026, 6, 22, 12, 0, 0, 0, loc).Unix(), end)
}

func TestMonitorHistoryWindowParserAcceptsTodayPresetSlot(t *testing.T) {
	orig := monitorNowFunc
	loc := monitorHistoryLoc
	monitorNowFunc = func() time.Time {
		return time.Date(2026, 6, 22, 12, 0, 0, 0, loc)
	}
	t.Cleanup(func() { monitorNowFunc = orig })

	start, end, ok := ResolveMonitorHistoryWindow(&TimeWindow{
		Type:  TimeWindowPreset,
		Value: "today",
	})

	require.True(t, ok)
	assert.Equal(t, time.Date(2026, 6, 22, 0, 0, 0, 0, loc).Unix(), start)
	assert.Equal(t, time.Date(2026, 6, 22, 12, 0, 0, 0, loc).Unix(), end)
}

func TestMonitorHistoryWindowParserRejectsUnsupportedRelativeSlots(t *testing.T) {
	orig := monitorNowFunc
	loc := monitorHistoryLoc
	monitorNowFunc = func() time.Time {
		return time.Date(2026, 6, 22, 12, 0, 0, 0, loc)
	}
	t.Cleanup(func() { monitorNowFunc = orig })

	cases := []string{
		"上周",
		"过去 3 months",
		"3 hours",
	}
	for _, value := range cases {
		t.Run(value, func(t *testing.T) {
			_, _, ok := ResolveMonitorHistoryWindow(&TimeWindow{
				Type:  TimeWindowRelative,
				Value: value,
			})
			assert.False(t, ok)
		})
	}
}

func TestMonitorHistoryWindowParserRejectsInvalidAbsoluteClockRange(t *testing.T) {
	orig := monitorNowFunc
	loc := monitorHistoryLoc
	monitorNowFunc = func() time.Time {
		return time.Date(2026, 6, 22, 12, 0, 0, 0, loc)
	}
	t.Cleanup(func() { monitorNowFunc = orig })

	_, _, ok := ResolveMonitorHistoryWindow(&TimeWindow{
		Type:  TimeWindowAbsolute,
		Value: "2026-06-21 10:00 到 09:00",
	})

	assert.False(t, ok, "invalid explicit range must not degrade to a whole-day range")
}
