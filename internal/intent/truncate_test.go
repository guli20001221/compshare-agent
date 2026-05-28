package intent

import (
	"testing"

	"github.com/compshare-agent/internal/entity"
	"github.com/stretchr/testify/assert"
)

func TestTruncateInstancesForDisplay_NoTruncationBelowLimit(t *testing.T) {
	in := []entity.InstanceSnapshot{
		{UHostId: "uhost-a", State: "Running"},
		{UHostId: "uhost-b", State: "Stopped"},
	}
	out, shown, truncated := TruncateInstancesForDisplay(in, 0)
	assert.False(t, truncated)
	assert.Equal(t, 2, shown)
	assert.Len(t, out, 2)
}

func TestTruncateInstancesForDisplay_TruncatesAbove10(t *testing.T) {
	in := make([]entity.InstanceSnapshot, 0, 47)
	for i := 0; i < 47; i++ {
		in = append(in, entity.InstanceSnapshot{
			UHostId:   "uhost-" + string(rune('a'+i%26)) + string(rune('a'+i/26)),
			State:     "Running",
			StartTime: int64(i),
		})
	}
	out, shown, truncated := TruncateInstancesForDisplay(in, 0)
	assert.True(t, truncated)
	assert.Equal(t, DefaultMaxInstancesPerDisplay, shown)
	assert.Len(t, out, DefaultMaxInstancesPerDisplay)
}

func TestSortInstancesForDisplay_StatePriorityThenStartTimeDesc(t *testing.T) {
	in := []entity.InstanceSnapshot{
		{UHostId: "old-stopped", State: "Stopped", StartTime: 100},
		{UHostId: "new-running", State: "Running", StartTime: 200},
		{UHostId: "old-running", State: "Running", StartTime: 100},
		{UHostId: "new-install", State: "Install", StartTime: 300},
	}
	SortInstancesForDisplay(in)
	assert.Equal(t, "new-running", in[0].UHostId, "Running with newer StartTime first")
	assert.Equal(t, "old-running", in[1].UHostId, "Running with older StartTime second")
	assert.Equal(t, "old-stopped", in[2].UHostId, "Stopped after Running")
	assert.Equal(t, "new-install", in[3].UHostId, "Install after Stopped")
}

func TestSortInstancesForDisplay_ZeroStartTimeSortsLastWithinState(t *testing.T) {
	in := []entity.InstanceSnapshot{
		{UHostId: "no-start", State: "Running", StartTime: 0},
		{UHostId: "has-start", State: "Running", StartTime: 100},
	}
	SortInstancesForDisplay(in)
	assert.Equal(t, "has-start", in[0].UHostId, "instance with StartTime should rank above StartTime=0")
}

func TestTruncateInstancesForDisplay_KeepsTopPriorityInstances(t *testing.T) {
	// 11 instances, only 2 Running — truncate should keep both Running ones.
	in := []entity.InstanceSnapshot{
		{UHostId: "stopped-1", State: "Stopped"},
		{UHostId: "stopped-2", State: "Stopped"},
		{UHostId: "stopped-3", State: "Stopped"},
		{UHostId: "stopped-4", State: "Stopped"},
		{UHostId: "stopped-5", State: "Stopped"},
		{UHostId: "stopped-6", State: "Stopped"},
		{UHostId: "stopped-7", State: "Stopped"},
		{UHostId: "stopped-8", State: "Stopped"},
		{UHostId: "stopped-9", State: "Stopped"},
		{UHostId: "running-a", State: "Running", StartTime: 1},
		{UHostId: "running-b", State: "Running", StartTime: 2},
	}
	out, shown, truncated := TruncateInstancesForDisplay(in, 0)
	assert.True(t, truncated)
	assert.Equal(t, 10, shown)
	assert.Equal(t, "running-b", out[0].UHostId, "newest Running first")
	assert.Equal(t, "running-a", out[1].UHostId, "older Running second")
	// Both Running instances must survive truncation
	uhostIds := make([]string, 0, len(out))
	for _, s := range out {
		uhostIds = append(uhostIds, s.UHostId)
	}
	assert.Contains(t, uhostIds, "running-a")
	assert.Contains(t, uhostIds, "running-b")
}
