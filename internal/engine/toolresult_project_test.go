package engine

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/compshare-agent/internal/llm"
	"github.com/compshare-agent/internal/prompt"
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
	requireProjectionMetadata(t, result)
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
	assert.NotContains(t, result, agentResultProjectionMetadataKey)
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
	assertFormattedProjectionMetadata(t, flagOn)
	traceJSON, err := json.Marshal(events[len(events)-1].TraceResult)
	require.NoError(t, err)
	assert.Contains(t, string(traceJSON), "HugeMatrix",
		"projection must not mutate TraceResult")
}

// Canonical transcript is a replay of the MODEL-visible tool result, not of
// TraceResult. When the ReAct projection removes fields before FormatToolResult,
// the same omission marker must survive capture, storage and projection back
// into a future request. Otherwise a later model reads a partial catalog as if
// it were complete.
func TestProjectedToolResultMarksCanonicalTranscript(t *testing.T) {
	prev := canonicalTranscriptEnabled
	SetCanonicalTranscriptEnabled(true)
	defer SetCanonicalTranscriptEnabled(prev)

	result := map[string]any{
		"RetCode": 0,
		"ImageSet": []any{map[string]any{
			"CompShareImageId":   "img-1",
			"CompShareImageName": "PyTorch 2.1",
			"ImageType":          "App",
			"Description":        strings.Repeat("long", 200),
		}},
	}
	require.True(t, projectToolResultForReAct("DescribeCompShareImages", result),
		"precondition: this fixture must exercise a real projection")
	liveToolResult := prompt.FormatToolResult(result)
	assertFormattedProjectionMetadata(t, liveToolResult)

	call := openai.ToolCall{
		ID:   "call-projected",
		Type: openai.ToolTypeFunction,
		Function: openai.FunctionCall{
			Name:      "DescribeCompShareImages",
			Arguments: `{}`,
		},
	}
	e := &Engine{messages: []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "sys"},
		{Role: openai.ChatMessageRoleUser, Content: "有哪些镜像？"},
		{Role: openai.ChatMessageRoleAssistant, ToolCalls: []openai.ToolCall{call}},
		{Role: openai.ChatMessageRoleTool, ToolCallID: call.ID, Content: liveToolResult},
		{Role: openai.ChatMessageRoleAssistant, Content: "找到了一个镜像。"},
	}}
	e.captureTurnTranscript()
	payload, stats := e.LastTurnTranscript()
	require.True(t, stats.Attempted)
	require.NotNil(t, payload)

	stored := ParseTranscriptMetadata(payload)
	require.NotNil(t, stored)
	storedToolResult := transcriptToolContent(t, stored)
	assert.JSONEq(t, liveToolResult, storedToolResult,
		"canonical storage must preserve the model-visible projected result")
	assertFormattedProjectionMetadata(t, storedToolResult)

	replayed := ProjectTranscript(stored)
	var replayedToolResult string
	for _, msg := range replayed {
		if msg.Role == openai.ChatMessageRoleTool {
			replayedToolResult = msg.Content
			break
		}
	}
	require.NotEmpty(t, replayedToolResult)
	assert.JSONEq(t, liveToolResult, replayedToolResult,
		"cold replay must carry the same omission marker as the live turn")
	assertFormattedProjectionMetadata(t, replayedToolResult)
}

func requireProjectionMetadata(t *testing.T, result map[string]any) {
	t.Helper()
	metadata, ok := result[agentResultProjectionMetadataKey].(map[string]any)
	require.True(t, ok, "projected results must carry %s", agentResultProjectionMetadataKey)
	assert.Equal(t, true, metadata["applied"])
	assert.Equal(t, true, metadata["omitted_content"])
	assert.Equal(t, agentResultProjectionMetadataNote, metadata["notice"])
}

func assertFormattedProjectionMetadata(t *testing.T, formatted string) {
	t.Helper()
	var result map[string]any
	require.NoError(t, json.Unmarshal([]byte(formatted), &result), "formatted result must remain JSON")
	requireProjectionMetadata(t, result)
}

func transcriptToolContent(t *testing.T, transcript *TranscriptV1) string {
	t.Helper()
	for _, msg := range transcript.Messages {
		if msg.Role == string(openai.ChatMessageRoleTool) {
			return msg.Content
		}
	}
	t.Fatal("transcript contains no tool result")
	return ""
}

func jsonSize(t *testing.T, v any) int {
	t.Helper()
	data, err := json.Marshal(v)
	require.NoError(t, err)
	return len(data)
}
