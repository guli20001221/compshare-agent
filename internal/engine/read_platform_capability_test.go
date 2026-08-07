package engine

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/compshare-agent/internal/capability"
	"github.com/compshare-agent/internal/intent"
	"github.com/compshare-agent/internal/platform"
	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGenericReadPlatformCapabilityIsRemoved(t *testing.T) {
	_, ok := capability.ReadIntentForTool("ReadPlatformCapability")
	require.False(t, ok)
}

func TestOrdinaryReadReturnsObservationAndDoesNotEndTurn(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {
			"TotalCount": float64(1),
			"UHostSet": []any{map[string]any{
				"UHostId": "uhost-1", "Name": "train-a", "State": "Running",
				"GpuType": "4090", "GPU": float64(1), "CPU": float64(8), "Memory": float64(64),
			}},
		},
	}}
	eng := NewWithDeps(&mockLLM{}, executor, nil)
	out := eng.executeTool(context.Background(), toolCall("read", capability.ReadToolName(intent.IntentResourceInfo),
		`{}`), noopStep)
	_, ok := isFinalReply(out)
	require.False(t, ok, "a read capability is an observation and must never end the turn")
	var observation ReadCapabilityObservation
	require.NoError(t, json.Unmarshal([]byte(out), &observation))
	require.Equal(t, platform.ReadStatusHandled, observation.Status)
	require.NotNil(t, observation.Envelope)
	// Byte-identity guard for the engine-bridge migration (intent -> platform
	// status/route types): the wire strings are unchanged from the pre-migration
	// intent-typed observation, so the model sees the same JSON.
	assert.Contains(t, out, `"status":"handled"`)
	assert.Contains(t, out, `"route_status":"dispatched"`)
}

func TestInstanceAccessDiagnosisCanContinueToAgentAndKnowledge(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {
			"TotalCount": float64(1),
			"UHostSet": []any{map[string]any{
				"UHostId": "cpod-1", "Name": "pod-a", "State": "Running",
				"InstanceType": "Container",
				"Ports": map[string]any{
					"TcpPorts": []any{float64(22)},
				},
			}},
		},
	}}
	eng := NewWithDeps(&mockLLM{}, executor, nil)
	eng.lastUserMsg = "cpod-1 的 8188 端口打不开，怎么修"

	out := eng.executeTool(context.Background(), toolCall("read",
		capability.ReadToolName(intent.IntentInstanceAccess),
		`{"targets":[{"type":"uhost_id_user_input","value":"cpod-1","source":"user_text"}],"access_type":"custom_port","protocol":"tcp","port":8188}`), noopStep)

	_, final := isFinalReply(out)
	require.False(t, final, "a diagnosis is evidence for the Agent, not a reason to terminate its turn")
	var observation ReadCapabilityObservation
	require.NoError(t, json.Unmarshal([]byte(out), &observation))
	require.Equal(t, platform.ReadStatusHandled, observation.Status)
	require.NotNil(t, observation.Envelope)
	require.Len(t, eng.platformReadEvidenceThisTurn, 1)
	require.Empty(t, eng.sensitiveRepliesThisTurn)
}

func TestJupyterTokenReturnsOpaqueObservation(t *testing.T) {
	const token = "stable-console-visible-token"
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {
			"TotalCount": float64(1),
			"UHostSet": []any{map[string]any{
				"UHostId": "uhost-1", "Name": "vm-a", "State": "Running",
				"InstanceType": "UHost",
			}},
		},
		"DescribeCompShareJupyterToken": {"JupyterToken": token},
	}}
	eng := NewWithDeps(&mockLLM{}, executor, nil)
	eng.lastUserMsg = "查询 uhost-1 的 Jupyter Token"

	out := eng.executeTool(context.Background(), toolCall("read",
		capability.ReadToolName(intent.IntentInstanceAccess),
		`{"targets":[{"type":"uhost_id_user_input","value":"uhost-1","source":"user_text"}],"access_type":"jupyter_token"}`), noopStep)

	_, final := isFinalReply(out)
	require.False(t, final, "an opaque value must not terminate the central Agent")
	var observation ReadCapabilityObservation
	require.NoError(t, json.Unmarshal([]byte(out), &observation))
	require.Equal(t, platform.ReadStatusHandled, observation.Status)
	require.Contains(t, observation.Guidance, "敏感访问凭据")
	require.NotContains(t, out, token, "the opaque value must not pass through the model")
	require.Len(t, eng.platformReadEvidenceThisTurn, 1)
	require.Contains(t, eng.sensitiveRepliesThisTurn[0], token)
}

func TestRecentPriorUserTextsExcludesCurrentTurnAndAssistantText(t *testing.T) {
	eng := &Engine{lastUserMsg: "当前轮 那个呢", messages: []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "system"},
		{Role: openai.ChatMessageRoleUser, Content: "第一轮 InfiniteTalk"},
		{Role: openai.ChatMessageRoleAssistant, Content: "assistant-only LiveTalking"},
		{Role: openai.ChatMessageRoleUser, Content: "第二轮 ComfyUI"},
		{Role: openai.ChatMessageRoleUser, Content: "当前轮 那个呢"},
	}}
	require.Equal(t, []string{"第二轮 ComfyUI", "第一轮 InfiniteTalk"}, eng.recentPriorUserTexts(4))
}

func TestRecentPriorUserTextsExcludesScreenshotOCRAndWrappedCurrentTurn(t *testing.T) {
	current := "请推荐别的数字人镜像"
	eng := &Engine{lastUserMsg: current, messages: []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "system"},
		{Role: openai.ChatMessageRoleUser, Content: WrapScreenshotContext("旧截图出现 LiveTalking", "上一轮请看截图")},
		{Role: openai.ChatMessageRoleAssistant, Content: "助手提到 HeyGem"},
		{Role: openai.ChatMessageRoleUser, Content: WrapScreenshotContext("本轮截图出现 MuseTalk", current)},
	}}

	prior := eng.recentPriorUserTexts(4)
	require.Equal(t, []string{"上一轮请看截图"}, prior)
	require.Error(t, capability.ValidateCurrentTurnGrounding(
		capability.ImageListRequest{
			Source: platform.ImageSourceCommunity,
			Query:  "LiveTalking",
			Mode:   platform.ListModeFiltered,
		},
		current,
		prior...,
	))
	require.NoError(t, capability.ValidateCurrentTurnGrounding(
		capability.ImageListRequest{
			Source: platform.ImageSourceCommunity,
			Query:  "数字人镜像",
			Mode:   platform.ListModeFiltered,
		},
		current,
		prior...,
	))
}

func TestConcreteReadReturnsStructuredMissingFieldsBeforeHandler(t *testing.T) {
	executor := &mockExecutor{}
	eng := NewWithDeps(&mockLLM{}, executor, nil)
	out := eng.executeTool(context.Background(), toolCall("read", capability.ReadToolName(intent.IntentPricingQuery), `{}`), noopStep)

	var observation ReadCapabilityObservation
	require.NoError(t, json.Unmarshal([]byte(out), &observation))
	require.Equal(t, platform.ReadStatusNeedsInput, observation.Status)
	require.Equal(t, []capability.MissingField{{Name: "gpu_type", Reason: "required"}}, observation.MissingFields)
	require.Empty(t, executor.calls, "缺失字段必须在能力边界返回，不能进入 handler 或上游 API")
}

// TestAccountFinanceUnavailableReturnsStructuredUnavailable: the model-visible
// account-finance tool returns a structured Unavailable observation (status +
// reason + alternatives) without ever calling an upstream API, so a balance /
// invoice question gets a deterministic non-fabricated answer.
func TestAccountFinanceUnavailableReturnsStructuredUnavailable(t *testing.T) {
	executor := &mockExecutor{}
	eng := NewWithDeps(&mockLLM{}, executor, nil)
	out := eng.executeTool(context.Background(), toolCall("read", capability.ReadToolName(intent.Intent("account_finance_status")), `{}`), noopStep)

	if _, ok := isFinalReply(out); ok {
		t.Fatal("an unavailable capability is an observation and must never end the turn")
	}
	var observation UnavailableCapabilityObservation
	require.NoError(t, json.Unmarshal([]byte(out), &observation))
	require.Equal(t, "unavailable", observation.Status)
	require.Equal(t, "account_finance_status", observation.Capability)
	require.Contains(t, observation.Reason, "不支持直接查询账号余额")
	require.NotEmpty(t, observation.Alternatives)
	require.Empty(t, executor.calls, "an unavailable capability must not call any upstream API")
}

// The model may carry a prior card into the next tool call through canonical
// transcript. The server must never rewrite an omitted GPU argument from prior
// state.
func TestStockReadLeavesNoCrossTurnReferent(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeAvailableCompShareInstanceTypes": {
			"AvailableInstanceTypes": []any{
				map[string]any{"Name": "4090", "Status": "SoldOut", "Zone": "cn-wlcb-01"},
				map[string]any{"Name": "A100", "Status": "SoldOut", "Zone": "cn-wlcb-01"},
			},
		},
	}}
	eng := NewWithDeps(&mockLLM{}, executor, nil)
	eng.SetSessionState(SessionState{SchemaVersion: SessionStateSchemaCurrent}, 1)

	named := eng.executeTool(context.Background(),
		toolCall("read", capability.ReadToolName(intent.IntentStockAvailability), `{"gpu_type":"4090"}`), noopStep)
	_, ok := isFinalReply(named)
	require.False(t, ok, "a read capability is an observation and must never end the turn")
	require.Contains(t, named, "4090", "premise: the named turn resolved to a single card")
	require.NotContains(t, named, "A100", "premise: and filtered the other one out")

	// The subject-eliding follow-up, as the model would send it when it did NOT
	// carry the card forward: no gpu_type at all.
	followUp := eng.executeTool(context.Background(),
		toolCall("read", capability.ReadToolName(intent.IntentStockAvailability), `{}`), noopStep)

	assert.Contains(t, followUp, "A100",
		"the unfiltered follow-up was still filtered to the previous turn's card; the server is "+
			"remembering what the user meant and editing the model's arguments to match")
	assert.Contains(t, followUp, "4090", "an unfiltered listing includes everything")

	// A minimal freshness record may exist, but it has no model argument to
	// substitute. The unfiltered response above is the observable contract.
}

func TestReadBoundaryRejectsUngroundedMonitorAbsoluteWindow(t *testing.T) {
	executor := &mockExecutor{}
	eng := NewWithDeps(&mockLLM{}, executor, nil)
	eng.lastUserMsg = "查询昨天的CPU历史监控"
	out := eng.executeTool(context.Background(), toolCall("read", capability.ReadToolName(intent.IntentMonitorHistory),
		`{"time_window":{"type":"absolute","start":"2026-07-18 00:00","end":"2026-07-19 00:00","source_span":"昨天"}}`), noopStep)

	assert.Contains(t, out, "ungrounded capability request")
	require.Empty(t, executor.calls, "an invented absolute date must be rejected before any upstream query")
}
