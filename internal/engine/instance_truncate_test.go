package engine

import (
	"testing"

	"github.com/compshare-agent/internal/intent"
	"github.com/stretchr/testify/assert"
)

func TestTruncateDescribeResultForReAct_NoTruncationUnderLimit(t *testing.T) {
	result := map[string]any{
		"UHostSet": []any{
			map[string]any{"UHostId": "uhost-a", "State": "Running"},
			map[string]any{"UHostId": "uhost-b", "State": "Stopped"},
		},
	}
	shown, total, truncated := truncateDescribeResultForReAct(nil, result)
	assert.False(t, truncated)
	assert.Equal(t, 2, shown)
	assert.Equal(t, 2, total)
	hosts, _ := result["UHostSet"].([]any)
	assert.Len(t, hosts, 2)
	_, hasFlag := result["Truncated"]
	assert.False(t, hasFlag, "Truncated flag should not be added when no truncation happens")
}

func TestTruncateDescribeResultForReAct_TruncatesAbove10AndKeepsRunning(t *testing.T) {
	rows := make([]any, 0, 15)
	for i := 0; i < 13; i++ {
		rows = append(rows, map[string]any{
			"UHostId": "uhost-stopped-" + string(rune('a'+i)),
			"State":   "Stopped",
		})
	}
	rows = append(rows,
		map[string]any{"UHostId": "uhost-running-1", "State": "Running", "StartTime": float64(100)},
		map[string]any{"UHostId": "uhost-running-2", "State": "Running", "StartTime": float64(200)},
	)
	result := map[string]any{"UHostSet": rows, "TotalCount": float64(15)}

	shown, total, truncated := truncateDescribeResultForReAct(nil, result)
	assert.True(t, truncated)
	assert.Equal(t, intent.DefaultMaxInstancesPerDisplay, shown)
	assert.Equal(t, 15, total)
	assert.Equal(t, true, result["Truncated"])
	assert.Equal(t, intent.DefaultMaxInstancesPerDisplay, result["Shown"])

	keptHosts, _ := result["UHostSet"].([]any)
	assert.Len(t, keptHosts, intent.DefaultMaxInstancesPerDisplay)

	keptIDs := make([]string, 0, len(keptHosts))
	for _, raw := range keptHosts {
		row := raw.(map[string]any)
		keptIDs = append(keptIDs, row["UHostId"].(string))
	}
	assert.Equal(t, "uhost-running-2", keptIDs[0], "newest Running first")
	assert.Equal(t, "uhost-running-1", keptIDs[1], "older Running second")
	assert.Contains(t, keptIDs, "uhost-running-1", "both Running must survive truncation")
	assert.Contains(t, keptIDs, "uhost-running-2")
}

func TestTruncateDescribeResultForReAct_PinnedUHostIdsSkipsTruncation(t *testing.T) {
	rows := make([]any, 0, 12)
	for i := 0; i < 12; i++ {
		rows = append(rows, map[string]any{"UHostId": "uhost-" + string(rune('a'+i)), "State": "Running"})
	}
	result := map[string]any{"UHostSet": rows}
	args := map[string]any{"UHostIds": []any{"uhost-a", "uhost-b"}}

	shown, total, truncated := truncateDescribeResultForReAct(args, result)
	assert.False(t, truncated, "pinned UHostIds must skip truncation — user already chose targets")
	assert.Equal(t, 0, shown)
	assert.Equal(t, 0, total)
	hosts, _ := result["UHostSet"].([]any)
	assert.Len(t, hosts, 12, "list should be untouched")
}

func TestTruncateDescribeResultForReAct_NilResult(t *testing.T) {
	shown, total, truncated := truncateDescribeResultForReAct(nil, nil)
	assert.False(t, truncated)
	assert.Equal(t, 0, shown)
	assert.Equal(t, 0, total)
}

func TestTruncateDescribeResultForReAct_HandlesMalformedRows(t *testing.T) {
	rows := []any{
		map[string]any{"UHostId": "good-1", "State": "Running"},
		"not-a-map",
		map[string]any{"UHostId": "good-2", "State": "Stopped"},
	}
	result := map[string]any{"UHostSet": rows}
	shown, _, truncated := truncateDescribeResultForReAct(nil, result)
	assert.False(t, truncated, "3 raw entries below limit even with one bad row")
	assert.Equal(t, 3, shown)
}
