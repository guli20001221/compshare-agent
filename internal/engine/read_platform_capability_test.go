package engine

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/compshare-agent/internal/capability"
	"github.com/compshare-agent/internal/envelope"
	"github.com/compshare-agent/internal/intent"
	"github.com/compshare-agent/internal/platform"
	"github.com/compshare-agent/internal/tools"
	"github.com/compshare-agent/internal/zones"
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

func TestResourceInfoReceivesTheDeclaredLiveZoneCatalog(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareSupportZone": {"ZoneInfo": []any{
			map[string]any{"Zone": "cn-wlcb-01", "Region": "cn-wlcb", "ZoneId": float64(10027), "Describe": "华北二A"},
			map[string]any{"Zone": "cn-bj2-03", "Region": "cn-bj2", "ZoneId": float64(5001), "Describe": "华北一C", "IsPod": true},
		}},
		"DescribeCompShareInstance": {
			"TotalCount": float64(1),
			"UHostSet": []any{map[string]any{
				"UHostId": "uhost-zone", "Name": "zone-probe", "State": "Running",
				"Zone": "cn-wlcb-01", "Region": "cn-wlcb", "GPU": float64(0),
				"CPU": float64(2), "Memory": float64(4096),
			}},
		},
	}}
	eng := NewWithDeps(&mockLLM{}, executor, nil)
	eng.zoneCatalog = zones.NewCatalog(0)

	out := eng.executeTool(context.Background(), toolCall("read",
		capability.ReadToolName(intent.IntentResourceInfo), `{}`), noopStep)

	var observation ReadCapabilityObservation
	require.NoError(t, json.Unmarshal([]byte(out), &observation), out)
	require.Equal(t, platform.ReadStatusHandled, observation.Status)
	require.NotNil(t, observation.Envelope)
	assert.Equal(t, []string{"DescribeCompShareInstance", "DescribeCompShareSupportZone"}, observation.Envelope.SourceActions)
	assert.True(t, observation.Envelope.Constraints.DoNotInventZoneLabels)
	require.Len(t, eng.platformReadEvidenceThisTurn, 1)
	assert.Contains(t, eng.platformReadEvidenceThisTurn[0].Reply, "可用区 华北二A（cn-wlcb-01）")
	assert.NotContains(t, eng.platformReadEvidenceThisTurn[0].Reply, "华北一C")

	var zoneName any
	for _, fact := range observation.Envelope.Facts {
		if fact.SubjectID == "uhost-zone" && fact.Key == "zone_display_name" {
			zoneName = fact.Value
		}
	}
	assert.Equal(t, "华北二A", zoneName)
	assert.Contains(t, executor.calls, "DescribeCompShareSupportZone")
	assert.Contains(t, executor.calls, "DescribeCompShareInstance")
}

func TestResourceInfoStillReturnsRawZoneWhenTheCatalogIsUnavailable(t *testing.T) {
	executor := &mockExecutorFn{fn: func(action string, _ map[string]any) (map[string]any, error) {
		switch action {
		case "DescribeCompShareSupportZone":
			return nil, errors.New("zone catalog unavailable")
		case "DescribeCompShareInstance":
			return map[string]any{
				"TotalCount": float64(1),
				"UHostSet": []any{map[string]any{
					"UHostId": "uhost-zone", "Name": "zone-probe", "State": "Running",
					"Zone": "cn-wlcb-01", "Region": "cn-wlcb", "GPU": float64(0),
					"CPU": float64(2), "Memory": float64(4096),
				}},
			}, nil
		default:
			return nil, errors.New("unexpected action: " + action)
		}
	}}
	eng := NewWithDeps(&mockLLM{}, executor, nil)
	eng.zoneCatalog = zones.NewCatalog(0)

	out := eng.executeTool(context.Background(), toolCall("read",
		capability.ReadToolName(intent.IntentResourceInfo), `{}`), noopStep)

	var observation ReadCapabilityObservation
	require.NoError(t, json.Unmarshal([]byte(out), &observation), out)
	require.Equal(t, platform.ReadStatusHandled, observation.Status,
		"a display-name dependency outage must not hide otherwise valid instance facts")
	require.NotNil(t, observation.Envelope)
	assert.Equal(t, []string{"DescribeCompShareInstance"}, observation.Envelope.SourceActions)
	assert.True(t, observation.Envelope.Constraints.DoNotInventZoneLabels)

	facts := map[string]any{}
	for _, fact := range observation.Envelope.Facts {
		facts[fact.Key] = fact.Value
	}
	assert.Equal(t, "cn-wlcb-01", facts["zone"])
	assert.NotContains(t, facts, "zone_display_name")
	require.Len(t, eng.platformReadEvidenceThisTurn, 1)
	assert.Contains(t, eng.platformReadEvidenceThisTurn[0].Reply, "可用区 cn-wlcb-01")
	assert.NotContains(t, eng.platformReadEvidenceThisTurn[0].Reply, "华北")
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

	result, ok := tools.ParseAgentToolResult(agentToolObservation(capability.ReadToolName(intent.IntentPricingQuery), out))
	require.True(t, ok)
	require.Equal(t, tools.AgentToolNextAskUser, result.NextStep,
		"a real missing user field must stay on the user-clarification branch")
}

func TestRejectedReadArgumentsAskTheModelToCorrectItsOwnCall(t *testing.T) {
	for _, tc := range []struct {
		name         string
		lastUser     string
		action       string
		arguments    string
		sourceStatus string
	}{
		{
			name:         "schema validation",
			lastUser:     "查询 uhost-diag-002 的 SSH 登录方式",
			action:       capability.ReadToolName(intent.IntentInstanceAccess),
			arguments:    `{"targets":[{"type":"uhost_id_user_input","value":"uhost-diag-002","source":"user_text"}],"access_type":"ssh","evil":"injection"}`,
			sourceStatus: "read_argument_validation",
		},
		{
			name:         "invented grounding filter",
			lastUser:     "查询昨天的CPU历史监控",
			action:       capability.ReadToolName(intent.IntentMonitorHistory),
			arguments:    `{"time_window":{"type":"absolute","start":"2026-07-18 00:00","end":"2026-07-19 00:00","source_span":"昨天"}}`,
			sourceStatus: "read_argument_grounding",
		},
		{
			name:         "catalog zone without same-turn catalog evidence",
			lastUser:     "华北2a 的 H20 现在有库存吗",
			action:       capability.ReadToolName(intent.IntentStockAvailability),
			arguments:    `{"gpu_type":"H20","zone_mentions":["cn-wlcb-01"],"inventory_pool":"Unspecified"}`,
			sourceStatus: "read_argument_grounding",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			executor := &mockExecutor{}
			eng := NewWithDeps(&mockLLM{}, executor, nil)
			eng.lastUserMsg = tc.lastUser

			raw := eng.executeTool(context.Background(), toolCall("read", tc.action, tc.arguments), noopStep)
			result, ok := tools.ParseAgentToolResult(agentToolObservation(tc.action, raw))
			require.True(t, ok, raw)
			require.Equal(t, tools.AgentToolStatusNeedsInput, result.Status)
			require.Equal(t, tools.AgentToolCodeInvalidArguments, result.Error.Code)
			require.Equal(t, tools.AgentToolNextCorrectToolCall, result.NextStep,
				"the user did not need to add anything; the model owns this repair")
			require.False(t, result.Retryable)
			require.Equal(t, tc.sourceStatus, result.Meta.SourceStatus)
			require.Empty(t, executor.calls, "a rejected call must not reach an upstream read")
		})
	}
}

func TestUnmatchedStockZoneIsCorrectedFromSameTurnLiveCatalog(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeAvailableCompShareInstanceTypes": {
			"AvailableInstanceTypes": []any{map[string]any{
				"Name": "H20", "Zone": "cn-wlcb-01", "Status": "Normal",
				"Disks": []any{map[string]any{"BootDisk": []any{map[string]any{"Name": "CLOUD_SSD", "MinimalSize": float64(100)}}}},
			}},
		},
		"DescribeCompShareSupportZone": {"ZoneInfo": []any{
			map[string]any{"Zone": "cn-wlcb-01", "Region": "cn-wlcb", "ZoneId": float64(1), "Describe": "华北二A"},
			map[string]any{"Zone": "cn-sh2-02", "Region": "cn-sh2", "ZoneId": float64(2), "Describe": "上海二B"},
			map[string]any{"Zone": "cn-bj2-03", "Region": "cn-bj2", "ZoneId": float64(5001), "Describe": "华北一C", "IsPod": true},
			map[string]any{"Zone": "cn-wlcb-03", "Region": "cn-wlcb", "ZoneId": float64(10033), "Describe": "华北二C", "IsPod": true},
		}},
		"DescribeCompShareImages": {"ImageSet": []any{map[string]any{
			"CompShareImageId": "img-system", "Status": "Available", "ImageType": "System",
		}}},
		"CheckCompShareResourceCapacity": {"Specs": []any{map[string]any{
			"Gpu": float64(1), "Cpu": float64(16), "Mem": float64(64), "ResourceEnough": true,
		}}},
		"DescribeCompShareGpuInventory": {"GpuInventory": map[string]any{
			"Exclusive": map[string]any{"1": map[string]any{"H20": float64(1)}},
		}},
	}}
	eng := NewWithDeps(&mockLLM{}, executor, nil)
	eng.lastUserMsg = "华北2a 的 H20 现在有库存吗"
	action := capability.ReadToolName(intent.IntentStockAvailability)

	first := eng.executeTool(context.Background(), toolCall("stock-raw", action,
		`{"gpu_type":"H20","zone_mentions":["华北2a"],"inventory_pool":"Unspecified"}`), noopStep)
	correction, ok := tools.ParseAgentToolResult(first)
	require.True(t, ok, first)
	require.Equal(t, tools.AgentToolNextCorrectToolCall, correction.NextStep)
	require.Equal(t, tools.AgentToolCodeInvalidArguments, correction.Error.Code)
	data, ok := correction.Data.(map[string]any)
	require.True(t, ok)
	evidence, ok := data["evidence"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, string(envelope.KindZoneCatalog), evidence["kind"])
	require.Len(t, evidence["subjects"], 4, "the correction must carry the complete live catalog")
	require.Len(t, eng.platformReadEvidenceThisTurn, 1)

	second := eng.executeTool(context.Background(), toolCall("stock-canonical", action,
		`{"gpu_type":"H20","zone_mentions":["cn-wlcb-01"],"inventory_pool":"Unspecified"}`), noopStep)
	var observation ReadCapabilityObservation
	require.NoError(t, json.Unmarshal([]byte(second), &observation), second)
	require.Equal(t, platform.ReadStatusHandled, observation.Status)
	require.NotNil(t, observation.Envelope)
	require.Contains(t, executor.calls, "CheckCompShareResourceCapacity",
		"the corrected call must execute the requested stock query rather than stop at the catalog")
}

// TestAccountFinanceUnavailableReturnsStructuredUnavailable: the model-visible
// account-finance tool returns a structured Unavailable observation (status +
// reason + alternatives) without ever calling an upstream API, so a balance /
// account-ledger question gets a deterministic non-fabricated answer. Invoice
// status is a separate live typed read.
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

	result, ok := tools.ParseAgentToolResult(agentToolObservation(capability.ReadToolName(intent.IntentMonitorHistory), out))
	require.True(t, ok, out)
	assert.Equal(t, tools.AgentToolNextCorrectToolCall, result.NextStep)
	assert.Equal(t, tools.AgentToolCodeInvalidArguments, result.Error.Code)
	require.Empty(t, executor.calls, "an invented absolute date must be rejected before any upstream query")
}
