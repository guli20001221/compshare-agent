package readprojection

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMonitorHistoryWindowAcceptsStructuredAbsoluteRange(t *testing.T) {
	start, end, ok := ResolveMonitorHistoryWindow(&TimeWindow{
		Type:  TimeWindowAbsolute,
		Start: "2026-05-08 01:00",
		End:   "2026-05-08 02:00",
	})
	require.True(t, ok)
	assert.Equal(t, int64(1778173200), start)
	assert.Equal(t, int64(1778176800), end)
}

func TestMonitorHistoryWindowAcceptsRelativeHours(t *testing.T) {
	orig := monitorNowFunc
	loc := monitorHistoryLoc
	monitorNowFunc = func() time.Time { return time.Date(2026, 6, 22, 12, 0, 0, 0, loc) }
	t.Cleanup(func() { monitorNowFunc = orig })

	start, end, ok := ResolveMonitorHistoryWindow(&TimeWindow{
		Type: TimeWindowRelative, Amount: 3, Unit: "hour",
	})
	require.True(t, ok)
	assert.Equal(t, time.Date(2026, 6, 22, 9, 0, 0, 0, loc).Unix(), start)
	assert.Equal(t, time.Date(2026, 6, 22, 12, 0, 0, 0, loc).Unix(), end)
}

func TestMonitorHistoryWindowAcceptsRelativeMinutes(t *testing.T) {
	orig := monitorNowFunc
	loc := monitorHistoryLoc
	monitorNowFunc = func() time.Time { return time.Date(2026, 6, 22, 12, 0, 0, 0, loc) }
	t.Cleanup(func() { monitorNowFunc = orig })

	start, end, ok := ResolveMonitorHistoryWindow(&TimeWindow{
		Type: TimeWindowRelative, Amount: 30, Unit: "minute",
	})
	require.True(t, ok)
	assert.Equal(t, time.Date(2026, 6, 22, 11, 30, 0, 0, loc).Unix(), start)
	assert.Equal(t, time.Date(2026, 6, 22, 12, 0, 0, 0, loc).Unix(), end)
}

func TestMonitorHistoryWindowAcceptsThirtyDaysButNotMore(t *testing.T) {
	orig := monitorNowFunc
	loc := monitorHistoryLoc
	monitorNowFunc = func() time.Time { return time.Date(2026, 6, 30, 12, 0, 0, 0, loc) }
	t.Cleanup(func() { monitorNowFunc = orig })

	start, end, ok := ResolveMonitorHistoryWindow(&TimeWindow{
		Type: TimeWindowRelative, Amount: 30, Unit: "day",
	})
	require.True(t, ok)
	assert.Equal(t, time.Date(2026, 5, 31, 12, 0, 0, 0, loc).Unix(), start)
	assert.Equal(t, time.Date(2026, 6, 30, 12, 0, 0, 0, loc).Unix(), end)

	_, _, ok = ResolveMonitorHistoryWindow(&TimeWindow{
		Type: TimeWindowRelative, Amount: 31, Unit: "day",
	})
	assert.False(t, ok)
}

func TestMonitorHistoryWindowYesterdayIsComputedByServer(t *testing.T) {
	orig := monitorNowFunc
	loc := monitorHistoryLoc
	monitorNowFunc = func() time.Time { return time.Date(2026, 7, 20, 14, 0, 0, 0, loc) }
	t.Cleanup(func() { monitorNowFunc = orig })

	start, end, ok := ResolveMonitorHistoryWindow(&TimeWindow{
		Type: TimeWindowPreset, Preset: "yesterday",
	})
	require.True(t, ok)
	assert.Equal(t, time.Date(2026, 7, 19, 0, 0, 0, 0, loc).Unix(), start)
	assert.Equal(t, time.Date(2026, 7, 20, 0, 0, 0, 0, loc).Unix(), end)
}

func TestMonitorHistoryWindowRejectsMixedOrInvalidContracts(t *testing.T) {
	cases := []TimeWindow{
		{Type: TimeWindowRelative, Amount: 3, Unit: "month"},
		{Type: TimeWindowRelative, Amount: 0, Unit: "hour"},
		{Type: TimeWindowRelative, Amount: 3, Unit: "hour", Preset: "yesterday"},
		{Type: TimeWindowAbsolute, Start: "2026-06-21 10:00", End: "2026-06-21 09:00"},
		{Type: TimeWindowPreset, Preset: "yesterday", Start: "2025-01-01 00:00"},
		{Type: TimeWindowPreset, Preset: "yesterday", Timezone: "Mars/Base"},
	}
	for i := range cases {
		_, _, ok := ResolveMonitorHistoryWindow(&cases[i])
		assert.False(t, ok, "case %d", i)
	}
}
