package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/compshare-agent/internal/capability"
	"github.com/compshare-agent/internal/intent"
	"github.com/compshare-agent/internal/llm"
	"github.com/compshare-agent/internal/tools"
	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/require"
)

// instanceIDCorrectionModel deliberately emits a truncated target first. Its
// second call is built only from the latest tool observation, so deleting the
// structured correction cannot be masked by the full ID already present in the
// user message or by a pre-scripted response.
type instanceIDCorrectionModel struct {
	action      string
	shortID     string
	stage       int
	correctedID string
	calls       []llm.ChatRequest
}

func (m *instanceIDCorrectionModel) Chat(_ context.Context, req llm.ChatRequest) (*llm.ChatResponse, error) {
	m.calls = append(m.calls, req)
	switch m.stage {
	case 0:
		m.stage++
		return &llm.ChatResponse{ToolCalls: []openai.ToolCall{m.call("short", m.shortID)}}, nil
	case 1:
		m.stage++
		m.correctedID = completeInstanceIDFromLastToolObservation(req.Messages)
		if m.correctedID == "" {
			return &llm.ChatResponse{Content: "工具没有返回可用的完整实例 ID。"}, nil
		}
		return &llm.ChatResponse{ToolCalls: []openai.ToolCall{m.call("corrected", m.correctedID)}}, nil
	default:
		return &llm.ChatResponse{Content: "已按完整实例 ID 完成查询。"}, nil
	}
}

func (m *instanceIDCorrectionModel) call(id, instanceID string) openai.ToolCall {
	if m.action == "DiagnoseInstanceInternals" {
		return toolCall(id, m.action, fmt.Sprintf(`{"UHostId":%q,"Task":"排查 ComfyUI 无法打开","Mode":"inspect"}`, instanceID))
	}
	return toolCall(id, m.action, fmt.Sprintf(
		`{"targets":[{"type":"uhost_id_user_input","value":%q,"source":"user_text","source_span":%q}]}`,
		instanceID, instanceID,
	))
}

func completeInstanceIDFromLastToolObservation(messages []openai.ChatCompletionMessage) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role != openai.ChatMessageRoleTool {
			continue
		}
		result, ok := tools.ParseAgentToolResult(messages[i].Content)
		if !ok {
			return ""
		}
		data, ok := result.Data.(map[string]any)
		if !ok {
			return ""
		}
		value, _ := data["complete_instance_id"].(string)
		return strings.TrimSpace(value)
	}
	return ""
}

func TestChatCorrectsATruncatedReadTargetBeforeAnyUpstreamCall(t *testing.T) {
	const (
		fullID  = "uhost-1u8jtt7sral1"
		shortID = "uhost-1u8jtt7sral"
	)
	action := capability.ReadToolName(intent.IntentResourceInfo)
	model := &instanceIDCorrectionModel{action: action, shortID: shortID}
	var describedIDs [][]string
	exec := &mockExecutorFn{fn: func(action string, args map[string]any) (map[string]any, error) {
		if action != "DescribeCompShareInstance" {
			return map[string]any{"RetCode": 0}, nil
		}
		var ids []string
		switch raw := args["UHostIds"].(type) {
		case []string:
			ids = append(ids, raw...)
		case []any:
			for _, item := range raw {
				ids = append(ids, fmt.Sprint(item))
			}
		}
		describedIDs = append(describedIDs, ids)
		return map[string]any{"RetCode": 0, "TotalCount": 1, "UHostSet": []any{map[string]any{
			"UHostId": fullID, "Name": "test", "State": "Running",
		}}}, nil
	}}
	eng := NewWithDeps(model, exec, nil)

	reply, err := eng.Chat(context.Background(), "查询实例 "+fullID, noopStep)
	require.NoError(t, err)
	require.Equal(t, "已按完整实例 ID 完成查询。", reply)
	require.Equal(t, fullID, model.correctedID)
	require.Equal(t, [][]string{{fullID}}, describedIDs,
		"the rejected short call must not reach Describe; only the model's corrected call executes")
	require.GreaterOrEqual(t, len(model.calls), 2)

	var firstResult tools.AgentToolResult
	for _, message := range model.calls[1].Messages {
		if message.Role == openai.ChatMessageRoleTool && message.ToolCallID == "short" {
			require.NoError(t, json.Unmarshal([]byte(message.Content), &firstResult))
		}
	}
	require.Equal(t, tools.AgentToolNextCorrectToolCall, firstResult.NextStep)
	require.Equal(t, "instance_id_literal_truncated", firstResult.Meta.SourceStatus)
}

func TestChatCorrectsATruncatedInstanceOpsTargetBeforeEnteringTheInstance(t *testing.T) {
	const (
		fullID  = "uhost-1u8jtt7sral1"
		shortID = "uhost-1u8jtt7sral"
	)
	model := &instanceIDCorrectionModel{action: "DiagnoseInstanceInternals", shortID: shortID}
	runner := &fakeInstanceOpsRunner{verdict: InstanceOpsVerdict{Text: "排查完成", Ran: 1}}
	eng := NewWithDeps(model, &mockExecutor{}, nil)
	eng.SetInstanceOps(runner)

	reply, err := eng.Chat(context.Background(), "进去排查 "+fullID, noopStep)
	require.NoError(t, err)
	require.Equal(t, "排查完成", reply)
	require.Equal(t, fullID, model.correctedID)
	require.Equal(t, 1, runner.calls, "the truncated call must not consume the one runner attempt")
	require.Equal(t, fullID, runner.lastReq.InstanceID)
}

func TestInstanceIDCompletionDoesNotTurnAnotherMentionIntoTheTarget(t *testing.T) {
	const (
		mentioned = "uhost-current-123"
		carried   = "uhost-carried-456"
	)
	err := capability.ValidateUserLiteralInstanceID(carried, "实例 "+mentioned+" 已恢复，继续排查刚才那台")
	_, ok := modelOwnedInstanceIDCompletion("DiagnoseInstanceInternals", err)
	require.False(t, ok, "a different ID is not a suffix completion and must never be selected as a replacement")

	err = capability.ValidateUserLiteralInstanceID(
		"uhost-shared-",
		"对比 uhost-shared-first 和 uhost-shared-second",
	)
	_, ok = modelOwnedInstanceIDCompletion("DiagnoseInstanceInternals", err)
	require.False(t, ok, "a shared truncated prefix must not select one of multiple full IDs")
}
