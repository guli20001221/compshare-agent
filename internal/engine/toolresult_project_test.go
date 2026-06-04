package engine

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/compshare-agent/internal/llm"
	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProjectToolResultForReAct_AvailabilityKeepsGPUFieldsAndShrinks(t *testing.T) {
	result := map[string]any{
		"RetCode": 0,
		"AvailableInstanceTypes": []any{
			map[string]any{
				"Zone":       "cn-bj-01",
				"GPUType":    "RTX4090",
				"GPU":        float64(2),
				"CPU":        float64(32),
				"Memory":     float64(131072),
				"Status":     "Normal",
				"HugeMatrix": []any{strings.Repeat("x", 400), strings.Repeat("y", 400)},
			},
		},
	}
	before := jsonSize(t, result)

	assert.True(t, projectToolResultForReAct("DescribeAvailableCompShareInstanceTypes", result),
		"projection should report it fired")

	after := jsonSize(t, result)
	assert.Less(t, after, before)
	rows, ok := result["AvailableInstanceTypes"].([]any)
	require.True(t, ok)
	row := rows[0].(map[string]any)
	assert.Equal(t, "RTX4090", row["GPUType"])
	assert.Equal(t, float64(2), row["GPU"])
	assert.NotContains(t, row, "HugeMatrix")
}

func TestProjectToolResultForReAct_MonitorDropsNestedBulkKeepsNoData(t *testing.T) {
	result := map[string]any{
		"RetCode":              0,
		"MonitorDataStatus":    "NO_DATA_IN_REQUESTED_WINDOW",
		"MonitorDataGuidance":  "do not invent",
		"SshLoginCommand":      "ssh root@1.2.3.4",
		"DiskSet":              []any{map[string]any{"DiskId": "disk-1"}},
		"RawMonitorNestedBulk": []any{strings.Repeat("x", 300), strings.Repeat("y", 300)},
		"Data": map[string]any{
			"List": []any{
				map[string]any{
					"UHostId": "uhost-A",
					"Metric":  "cpu_usage",
					"Values":  []any{float64(10), float64(20), float64(30)},
				},
			},
		},
	}
	before := jsonSize(t, result)

	projectToolResultForReAct("GetCompShareInstanceMonitor", result)

	after := jsonSize(t, result)
	assert.Less(t, after, before)
	assert.Equal(t, "NO_DATA_IN_REQUESTED_WINDOW", result["MonitorDataStatus"])
	assert.Equal(t, "do not invent", result["MonitorDataGuidance"])
	assert.Equal(t, "ssh root@1.2.3.4", result["SshLoginCommand"])
	assert.Contains(t, result, "DiskSet")
	assert.NotContains(t, result, "RawMonitorNestedBulk")
	assert.Contains(t, result, "MonitorSummary")
}

func TestProjectToolResultForReAct_ImageListKeepsIDsAndNames(t *testing.T) {
	result := map[string]any{
		"RetCode": 0,
		"ImageSet": []any{
			map[string]any{
				"CompShareImageId":   "img-1",
				"CompShareImageName": "PyTorch 2.1",
				"ImageType":          "App",
				"OsType":             "Ubuntu",
				"Description":        strings.Repeat("long", 200),
			},
		},
	}
	before := jsonSize(t, result)

	projectToolResultForReAct("DescribeCompShareImages", result)

	after := jsonSize(t, result)
	assert.Less(t, after, before)
	rows := result["ImageSet"].([]any)
	row := rows[0].(map[string]any)
	assert.Equal(t, "img-1", row["CompShareImageId"])
	assert.Equal(t, "PyTorch 2.1", row["CompShareImageName"])
	assert.NotContains(t, row, "Description")
}

func TestProjectToolResultForReAct_MalformedInputDoesNotPanic(t *testing.T) {
	result := map[string]any{
		"RetCode":                0,
		"AvailableInstanceTypes": "not-a-list",
		"ImageSet":               []any{"not-a-map"},
	}

	require.NotPanics(t, func() {
		projectToolResultForReAct("DescribeAvailableCompShareInstanceTypes", result)
	})
	assert.Equal(t, 0, result["RetCode"])
}

func TestProjectToolResultForReAct_ReturnsFalseWhenNoOp(t *testing.T) {
	// Action not on the projection allowlist: no-op, no signal.
	assert.False(t, projectToolResultForReAct("DescribeCompShareInstance",
		map[string]any{"RetCode": 0, "InstanceSet": []any{}}))
	// Nil result: no-op, no signal.
	assert.False(t, projectToolResultForReAct("DescribeCompShareImages", nil))
}

func TestProjectToolResultForReAct_ReturnsFalseWhenNothingShrinks(t *testing.T) {
	// Eligible action, but the result is already minimal (only always-retained
	// fields, no bulky payload to drop). Projection must NOT claim it fired, and
	// must leave the result byte-for-byte unchanged — otherwise projected=true
	// is a false positive in the trace/regression signal.
	result := map[string]any{
		"RetCode":           float64(0),
		"MonitorDataStatus": "NO_DATA_IN_REQUESTED_WINDOW",
	}
	before := jsonSize(t, result)
	assert.False(t, projectToolResultForReAct("GetCompShareInstanceMonitor", result),
		"already-minimal result must not report projection")
	assert.Equal(t, before, jsonSize(t, result), "result must be unchanged on a no-op projection")
	assert.Equal(t, "NO_DATA_IN_REQUESTED_WINDOW", result["MonitorDataStatus"])
}

func TestExecuteTool_ProjectionFlagOnlyChangesLLMVisibleResult(t *testing.T) {
	fat := map[string]any{
		"RetCode": 0,
		"AvailableInstanceTypes": []any{
			map[string]any{
				"GPUType":    "RTX4090",
				"GPU":        float64(1),
				"HugeMatrix": []any{strings.Repeat("x", 500)},
			},
		},
	}
	exec := &mockExecutor{results: map[string]map[string]any{
		"DescribeAvailableCompShareInstanceTypes": fat,
	}}
	eng := NewWithDeps(&mockLLM{responses: []llm.ChatResponse{{Content: "ok"}}}, exec, nil)
	var events []StepEvent
	tc := openai.ToolCall{
		ID:   "call-1",
		Type: openai.ToolTypeFunction,
		Function: openai.FunctionCall{
			Name:      "DescribeAvailableCompShareInstanceTypes",
			Arguments: `{}`,
		},
	}

	flagOff := eng.executeTool(context.Background(), tc, func(ev StepEvent) { events = append(events, ev) })
	require.Contains(t, flagOff, "HugeMatrix")
	require.NotEmpty(t, events)
	require.Contains(t, events[len(events)-1].TraceResult, "AvailableInstanceTypes")
	assert.False(t, events[len(events)-1].Projected, "flag off: no projection signal")

	events = nil
	eng.SetReactResultProjectionEnabled(true)
	flagOn := eng.executeTool(context.Background(), tc, func(ev StepEvent) { events = append(events, ev) })

	assert.NotContains(t, flagOn, "HugeMatrix")
	assert.Contains(t, flagOn, "RTX4090")
	assert.True(t, events[len(events)-1].Projected, "flag on: projection signal recorded for trace")
	traceJSON, err := json.Marshal(events[len(events)-1].TraceResult)
	require.NoError(t, err)
	assert.Contains(t, string(traceJSON), "HugeMatrix",
		"projection must not mutate TraceResult")
}

func jsonSize(t *testing.T, v any) int {
	t.Helper()
	data, err := json.Marshal(v)
	require.NoError(t, err)
	return len(data)
}
