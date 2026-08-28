package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/compshare-agent/internal/llm"
	"github.com/compshare-agent/internal/tools"
	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/require"
)

// Address derivation fails before the lane enters the instance. The reply must preserve that
// evidence boundary: say what did not run, leave the underlying cause unresolved, and offer
// next steps the user can actually perform.
func TestInstanceOps_AddressUnavailableReportsTheUnresolvedEntryFailure(t *testing.T) {
	runner := &fakeInstanceOpsRunner{err: ErrInstanceOpsAddressUnavailable}
	eng := newInstanceOpsEngine(runner, alwaysConfirm)

	var steps []StepEvent
	out := eng.executeInstanceOps(context.Background(), "DiagnoseInstanceInternals", instanceOpsArgs(), captureSteps(&steps))

	require.True(t, strings.HasPrefix(out, finalReplyPrefix), "a failed rewrite is a terminal refusal")
	require.Contains(t, out, "没有进入实例")
	require.Contains(t, out, "没有执行任何实例内命令")
	require.Contains(t, out, "尚无法判断根因")
	require.Contains(t, out, "控制台", "the refusal must give the user an actionable fallback")
	require.NotContains(t, out, "与实例本身无关")
}

// Candidate addresses that all fail TCP preflight are not an address-derivation
// failure and do not prove which network, port, service, or instance layer failed.
// Crucially, it is a normal structured observation, so it cannot terminate the
// central Agent before it can reconcile a user's independent evidence.
func TestInstanceOps_SSHPreflightUnreachableReturnsStructuredVantageObservation(t *testing.T) {
	raw := newInstanceOpsEngine(
		&fakeInstanceOpsRunner{err: ErrInstanceOpsSSHPreflightUnreachable}, alwaysConfirm,
	).executeInstanceOps(context.Background(), "DiagnoseInstanceInternals", instanceOpsArgs(), noopStep)
	require.False(t, strings.HasPrefix(raw, finalReplyPrefix), "the central Agent must receive this observation")

	result, ok := tools.ParseAgentToolResult(raw)
	require.True(t, ok, raw)
	require.Equal(t, tools.AgentToolStatusFailed, result.Status)
	require.Equal(t, "SSH_DIAGNOSTIC_VANTAGE_UNREACHABLE", result.Error.Code)
	require.Equal(t, tools.AgentToolNextAnswerUser, result.NextStep)
	data, ok := result.Data.(map[string]any)
	require.True(t, ok, "%#v", result.Data)
	require.Equal(t, "diagnostic_service", data["observation_source"])
	require.Equal(t, "diagnostic_service_to_instance_ssh_candidate", data["observation_scope"])
	require.Equal(t, "pre_ssh_tcp", data["execution_boundary"])
	require.Equal(t, "candidate_available", data["ssh_entrypoint"])
	require.Equal(t, "not_established", data["tcp_connection"])
	require.Equal(t, false, data["ssh_session_established"])
	require.Equal(t, false, data["guest_commands_executed"])
	require.Contains(t, data["evidence_boundary"], "another vantage point")
	require.Contains(t, result.Error.Message, "不能据此否定用户")
	require.NotContains(t, raw, "网络 / 安全组未放通",
		"a failed TCP probe cannot select one unobserved cause")
}

// A user can already have evidence from a different path (the production case
// supplied an ssh -vvv transcript whose TCP phase reached "Connection
// established"). The diagnosis service's failed preflight is still useful, but
// it must reach the same central Agent as a bounded observation instead of
// replacing the conversation with a terminal canned reply.
func TestInstanceOps_SSHPreflightUnreachableDoesNotOverrideUserConnectivityEvidence(t *testing.T) {
	model := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{toolCall("preflight", "DiagnoseInstanceInternals", `{"UHostId":"uhost-1","Task":"排查 SSH 登录异常","Mode":"inspect"}`)}},
		{Content: "诊断服务所在网络未能连通候选 SSH 地址；但你提供的 SSH 日志已证明另一条路径完成了 TCP 建连，因此不能把两者混为同一个结论。"},
	}}
	eng := NewWithDeps(model, &mockExecutor{results: map[string]map[string]any{}}, alwaysConfirm)
	eng.SetInstanceOps(&fakeInstanceOpsRunner{err: ErrInstanceOpsSSHPreflightUnreachable})

	reply, err := eng.Chat(context.Background(), "uhost-1 的 ssh -vvv 日志显示 Connection established；请排查为什么后续 SSH 握手失败。", noopStep)
	require.NoError(t, err)
	require.Contains(t, reply, "另一条路径完成了 TCP 建连")
	require.Len(t, model.calls, 2, "preflight is a normal observation, so the central Agent gets a synthesis round")

	var observation string
	sawUserEvidence := false
	for _, message := range model.calls[1].Messages {
		if message.Role == openai.ChatMessageRoleUser && strings.Contains(message.Content, "Connection established") {
			sawUserEvidence = true
		}
		if message.Role == openai.ChatMessageRoleTool && message.ToolCallID == "preflight" {
			observation = message.Content
		}
	}
	require.True(t, sawUserEvidence, "the central Agent must retain the user's independent connectivity evidence")
	result, ok := tools.ParseAgentToolResult(observation)
	require.True(t, ok, observation)
	require.Equal(t, "SSH_DIAGNOSTIC_VANTAGE_UNREACHABLE", result.Error.Code)
}

// With no independent user evidence, the exact same structured observation is
// still returned. This pins that the fix is not a keyword-triggered exception:
// the central Agent decides how to answer from the conversation it actually has.
func TestInstanceOps_SSHPreflightUnreachableWithoutUserEvidenceUsesSameObservation(t *testing.T) {
	model := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{toolCall("preflight", "DiagnoseInstanceInternals", `{"UHostId":"uhost-1","Task":"排查无法登录","Mode":"inspect"}`)}},
		{Content: "诊断服务未能建立 SSH 前置 TCP 连接，尚无法据此判断实例内部根因。"},
	}}
	eng := NewWithDeps(model, &mockExecutor{results: map[string]map[string]any{}}, alwaysConfirm)
	eng.SetInstanceOps(&fakeInstanceOpsRunner{err: ErrInstanceOpsSSHPreflightUnreachable})

	reply, err := eng.Chat(context.Background(), "请排查 uhost-1 无法登录。", noopStep)
	require.NoError(t, err)
	require.Contains(t, reply, "尚无法据此判断实例内部根因")
	require.Len(t, model.calls, 2)
	for _, message := range model.calls[1].Messages {
		require.NotContains(t, message.Content, "Connection established")
	}
}

// A missing SSH entrypoint closes only the Guest execution lane. The central
// Agent must receive another model round and may use a platform read capability
// to answer what remains observable outside the Guest.
func TestInstanceOps_NoSSHTargetCanContinueWithPlatformRead(t *testing.T) {
	const instanceID = "uhost-no-ssh-001"
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {
			"TotalCount": float64(1),
			"UHostSet": []any{map[string]any{
				"UHostId": instanceID,
				"State":   "Running",
				"OsType":  "Windows",
			}},
		},
	}}
	model := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{toolCall("guest", "DiagnoseInstanceInternals", `{"UHostId":"`+instanceID+`","Task":"排查远程登录失败","Mode":"inspect"}`)}},
		{ToolCalls: []openai.ToolCall{toolCall("platform", "ReadCapability_instance_access", `{"targets":[{"type":"uhost_id_user_input","value":"`+instanceID+`","source":"user_text"}],"access_type":"ssh"}`)}},
		{Content: "实例当前为 Running，但平台没有提供 SSH 登录入口；本轮没有进入 Guest，也没有执行 Guest 命令。请改用平台支持的远程入口继续排查。"},
	}}
	eng := NewWithDeps(model, executor, alwaysConfirm)
	eng.SetInstanceOps(&fakeInstanceOpsRunner{err: ErrInstanceOpsNoSSHTarget})

	reply, err := eng.Chat(context.Background(), "请排查 "+instanceID+" 的远程登录失败。", noopStep)
	require.NoError(t, err)
	require.Contains(t, reply, "当前为 Running")
	require.Contains(t, reply, "没有提供 SSH 登录入口")
	require.Contains(t, executor.calls, "DescribeCompShareInstance",
		"no-SSH observation must not terminate the turn before platform reads")
	require.Len(t, model.calls, 3)

	var guestObservation string
	for _, message := range model.calls[1].Messages {
		if message.Role == openai.ChatMessageRoleTool && message.ToolCallID == "guest" {
			guestObservation = message.Content
		}
	}
	result, ok := tools.ParseAgentToolResult(guestObservation)
	require.True(t, ok, guestObservation)
	require.Equal(t, "INSTANCE_GUEST_SSH_UNAVAILABLE", result.Error.Code)
}
