package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/compshare-agent/internal/capability"
	"github.com/compshare-agent/internal/intent"
	"github.com/compshare-agent/internal/platform"
	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/require"
)

func TestReadTargetFromTranscriptIsNotLimitedByEarlierUserLiterals(t *testing.T) {
	const targetID = "uhost-trainer-b"
	var described []string
	executor := &mockExecutorFn{fn: func(action string, args map[string]any) (map[string]any, error) {
		if action == "DescribeCompShareSupportZone" {
			return map[string]any{"ZoneInfo": []any{}}, nil
		}
		require.Equal(t, "DescribeCompShareInstance", action)
		described = append(described, args["UHostIds"].([]string)...)
		return map[string]any{"RetCode": 0, "TotalCount": 1, "UHostSet": []any{
			map[string]any{"UHostId": targetID, "Name": "训练机", "State": "Running"},
		}}, nil
	}}
	eng := NewWithDeps(&mockLLM{}, executor, nil)
	eng.lastUserMsg = "再看看刚才列表里的训练机"
	eng.messages = []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleUser, Content: "查询 uhost-other-a"},
		{Role: openai.ChatMessageRoleAssistant, Content: "训练机：" + targetID},
		{Role: openai.ChatMessageRoleUser, Content: eng.lastUserMsg},
	}

	raw := eng.executeTool(context.Background(), toolCall("read", capability.ReadToolName(intent.IntentResourceInfo),
		fmt.Sprintf(`{"targets":[{"type":"uhost_id_user_input","value":%q}]}`, targetID)), noopStep)
	var observation ReadCapabilityObservation
	require.NoError(t, json.Unmarshal([]byte(raw), &observation), raw)
	require.Equal(t, platform.ReadStatusHandled, observation.Status)
	require.Equal(t, []string{targetID}, described)
	require.Contains(t, raw, targetID)
}

func TestHistoricalMonitorFollowupUsesAgentWindowWithoutAnOriginalQuote(t *testing.T) {
	for _, tc := range []struct {
		name   string
		window string
	}{
		{"yesterday", `{"type":"preset","preset":"yesterday"}`},
		{"absolute", `{"type":"absolute","start":"2026-07-18 01:00","end":"2026-07-18 02:00"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var monitorArgs map[string]any
			executor := &mockExecutorFn{fn: func(action string, args map[string]any) (map[string]any, error) {
				if action == "DescribeCompShareInstance" {
					return map[string]any{"RetCode": 0, "UHostSet": []any{
						map[string]any{"UHostId": "uhost-second", "Name": "第二台", "State": "Running"},
					}}, nil
				}
				require.Equal(t, "GetCompShareInstanceMonitor", action)
				monitorArgs = args
				return map[string]any{"RetCode": 0}, nil
			}}
			eng := NewWithDeps(&mockLLM{}, executor, nil)
			eng.lastUserMsg = "那另一台呢"
			eng.messages = []openai.ChatCompletionMessage{
				{Role: openai.ChatMessageRoleUser, Content: "看看昨天 uhost-first 的 CPU 监控"},
				{Role: openai.ChatMessageRoleUser, Content: eng.lastUserMsg},
			}
			raw := eng.executeTool(context.Background(), toolCall("monitor", capability.ReadToolName(intent.IntentMonitorHistory),
				`{"targets":[{"type":"uhost_id_user_input","value":"uhost-second"}],"metrics":["cpu"],"time_window":`+tc.window+`}`), noopStep)
			var observation ReadCapabilityObservation
			require.NoError(t, json.Unmarshal([]byte(raw), &observation), raw)
			require.Equal(t, platform.ReadStatusHandled, observation.Status)
			require.Equal(t, []string{"uhost-second"}, monitorArgs["UHostIds"])
			require.Less(t, monitorArgs["StartTime"].(int64), monitorArgs["EndTime"].(int64))
		})
	}
}

func TestReadDoesNotReplaceAnUnknownIDWithALongerUserLiteral(t *testing.T) {
	const (
		fullID  = "uhost-1u8jtt7sral1"
		shortID = "uhost-1u8jtt7sral"
	)
	var described []string
	executor := &mockExecutorFn{fn: func(action string, args map[string]any) (map[string]any, error) {
		if action == "DescribeCompShareSupportZone" {
			return map[string]any{"ZoneInfo": []any{}}, nil
		}
		require.Equal(t, "DescribeCompShareInstance", action)
		described = append(described, args["UHostIds"].([]string)...)
		return map[string]any{"RetCode": 0, "TotalCount": 0, "UHostSet": []any{}}, nil
	}}
	eng := NewWithDeps(&mockLLM{}, executor, nil)
	eng.lastUserMsg = "查询 " + fullID
	raw := eng.executeTool(context.Background(), toolCall("read", capability.ReadToolName(intent.IntentResourceInfo),
		fmt.Sprintf(`{"targets":[{"type":"uhost_id_user_input","value":%q}]}`, shortID)), noopStep)
	var observation ReadCapabilityObservation
	require.NoError(t, json.Unmarshal([]byte(raw), &observation), raw)
	require.Equal(t, platform.ReadStatusEmpty, observation.Status)
	require.Equal(t, []string{shortID}, described)
	require.NotContains(t, raw, fullID)
}
