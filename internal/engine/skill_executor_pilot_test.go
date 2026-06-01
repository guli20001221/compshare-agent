package engine

import (
	"context"
	"testing"

	"github.com/compshare-agent/internal/llm"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests pin the P2a routing fork: with USE_SKILL_EXECUTOR on, the piloted
// DiagnosePortOrFirewall action runs through the body-driven orchestrator loop
// (the LLM drives read tools); with it off (shipped default), the deterministic
// Go chain runs and never touches the LLM. The flag is the only difference.

func TestExecuteDiagnosis_FlagOn_RoutesThroughSkillExecutor(t *testing.T) {
	prev := SkillExecutorEnabled()
	SetSkillExecutorEnabled(true)
	defer SetSkillExecutorEnabled(prev)

	exec := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {"UHostSet": []any{map[string]any{
			"UHostId": "u1", "State": "Running",
			"Softwares": []any{map[string]any{"Name": "JupyterLab", "URL": "http://1.2.3.4:8888"}},
		}}},
	}}
	// agentLLMClient is nil under NewWithDeps, so the executor falls back to this
	// llmClient. Responses are the skill loop's own turns: pick a read tool, then
	// finalize once it has seen the result.
	mock := &mockLLM{responses: []llm.ChatResponse{
		{Content: `{"action":"DescribeCompShareInstance","args":{"UHostIds":["u1"]}}`},
		{Content: `{"final":"实例运行中，JupyterLab 在 8888 端口可访问。"}`},
	}}
	eng := NewWithDeps(mock, exec, nil)
	eng.Init(context.Background())
	exec.calls = nil // drop Init's warm-up reads; count only the skill loop's calls

	reply := eng.executeDiagnosis(context.Background(), "DiagnosePortOrFirewall",
		map[string]any{"UHostId": "u1", "Service": "JupyterLab"}, func(StepEvent) {})

	assert.Equal(t, "实例运行中，JupyterLab 在 8888 端口可访问。", reply,
		"flag-on returns the skill loop's final answer, not a Go-chain DiagResult JSON")
	assert.Equal(t, []string{"DescribeCompShareInstance"}, exec.calls,
		"the skill loop drove exactly the read tool the model chose")
	require.GreaterOrEqual(t, mock.callIdx, 2, "the body-driven loop made its own LLM calls")
}

func TestExecuteDiagnosis_FlagOff_UsesGoChain(t *testing.T) {
	prev := SkillExecutorEnabled()
	SetSkillExecutorEnabled(false)
	defer SetSkillExecutorEnabled(prev)

	exec := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance":     {"UHostSet": []any{map[string]any{"UHostId": "u1", "State": "Running"}}},
		"DescribeCompShareSoftwarePort": {"SoftwarePort": []any{map[string]any{"Software": "JupyterLab", "Port": float64(8888)}}},
	}}
	mock := &mockLLM{} // the Go chain is deterministic and must not call the LLM
	eng := NewWithDeps(mock, exec, nil)
	eng.Init(context.Background())

	reply := eng.executeDiagnosis(context.Background(), "DiagnosePortOrFirewall",
		map[string]any{"UHostId": "u1", "Service": "JupyterLab"}, func(StepEvent) {})

	assert.NotEmpty(t, reply)
	assert.Equal(t, 0, mock.callIdx, "the Go chain path makes zero LLM calls — proves the flag-off branch")
}

// TestPilotSkillForDiagnosis_MapsExactlyReadOnlyDiagnoseActions pins the P3b-1
// pilot set: exactly the five READ-ONLY Diagnose* tool actions route to their
// agent-tier skill, every mapped skill resolves in the generated registry, and
// nothing else (DiagnoseBilling, a mutating action, the empty/unknown action) is
// piloted. This is the set-equality ceiling — it fails CI rather than letting the
// pilot silently widen to a mutating or unmapped action when the map is edited.
func TestPilotSkillForDiagnosis_MapsExactlyReadOnlyDiagnoseActions(t *testing.T) {
	want := map[string]string{
		"DiagnoseSSH":            "diagnose_ssh",
		"DiagnoseInitFailure":    "diagnose_init_failure",
		"DiagnoseGPU":            "diagnose_gpu_not_detected",
		"DiagnoseImageIssue":     "diagnose_image_issue",
		"DiagnosePortOrFirewall": "diagnose_port_firewall",
	}
	for action, skill := range want {
		got, piloted := pilotSkillForDiagnosis(action)
		assert.Truef(t, piloted, "%s must be piloted", action)
		assert.Equalf(t, skill, got, "%s mapped to the wrong skill", action)
		// Table↔registry binding: the value must be a real generated skill, so a
		// rename or typo fails here instead of degrading to the Go chain at runtime.
		_, ok := findGeneratedSkill(skill)
		assert.Truef(t, ok, "piloted skill %q is not in the generated registry", skill)
	}
	for _, action := range []string{"DiagnoseBilling", "StartInstanceWorkflow", "", "DiagnoseUnknown"} {
		got, piloted := pilotSkillForDiagnosis(action)
		assert.Falsef(t, piloted, "%s must NOT be piloted", action)
		assert.Emptyf(t, got, "%s must map to no skill", action)
	}
}

// TestExecuteDiagnosis_FlagOn_InitFailureGuardStillGates is the regression test for
// the P3b-1 guard-ordering fix. Before the fix the pilot ran at the top of
// executeDiagnosis, ahead of the DiagnoseInitFailure vague-symptom guard; extending
// the pilot to DiagnoseInitFailure would then have let the body executor run on a
// vague symptom, silently bypassing the guard. With the pilot now placed AFTER the
// guards, a vague init symptom must still be intercepted with the clarification and
// the executor (LLM) must never be reached — even with the flag on.
func TestExecuteDiagnosis_FlagOn_InitFailureGuardStillGates(t *testing.T) {
	prev := SkillExecutorEnabled()
	SetSkillExecutorEnabled(true)
	defer SetSkillExecutorEnabled(prev)

	exec := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {"UHostSet": []any{map[string]any{"UHostId": "u1", "State": "Install Fail"}}},
	}}
	// If the guard were bypassed, the pilot loop would consume these responses.
	mock := &mockLLM{responses: []llm.ChatResponse{
		{Content: `{"action":"DescribeCompShareInstance","args":{"UHostIds":["u1"]}}`},
		{Content: `{"final":"should never be reached"}`},
	}}
	eng := NewWithDeps(mock, exec, nil)
	eng.Init(context.Background())
	exec.calls = nil
	eng.lastUserMsg = "跑崩了" // vague fault language — NOT an init-failure signal

	reply := eng.executeDiagnosis(context.Background(), "DiagnoseInitFailure",
		map[string]any{"UHostId": "u1"}, func(StepEvent) {})

	assert.Contains(t, reply, "请问是哪台实例出了问题",
		"vague symptom must hit the Gate-1 clarification, not the body executor")
	assert.Equal(t, 0, mock.callIdx, "the executor (LLM) must never run when the init-failure guard fires")
	assert.Empty(t, exec.calls, "no diagnosis tool calls when the guard intercepts")
}

// TestExecuteDiagnosis_FlagOn_InitFailureGuardPasses_RoutesThroughExecutor is the
// positive half: once the DiagnoseInitFailure guards pass (specific init symptom +
// a named target), the now-extended pilot routes the turn through the body-driven
// executor instead of the Go chain.
func TestExecuteDiagnosis_FlagOn_InitFailureGuardPasses_RoutesThroughExecutor(t *testing.T) {
	prev := SkillExecutorEnabled()
	SetSkillExecutorEnabled(true)
	defer SetSkillExecutorEnabled(prev)

	exec := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {"UHostSet": []any{map[string]any{"UHostId": "u1", "State": "Install Fail"}}},
	}}
	mock := &mockLLM{responses: []llm.ChatResponse{
		{Content: `{"action":"DescribeCompShareInstance","args":{"UHostIds":["u1"]}}`},
		{Content: `{"final":"实例 u1 处于 Install Fail，初始化失败，建议删除重建。"}`},
	}}
	eng := NewWithDeps(mock, exec, nil)
	eng.Init(context.Background())
	exec.calls = nil
	eng.lastUserMsg = "我的实例初始化失败了" // contains an init-failure signal → Gate 1 passes

	reply := eng.executeDiagnosis(context.Background(), "DiagnoseInitFailure",
		map[string]any{"UHostId": "u1"}, func(StepEvent) {})

	assert.Equal(t, "实例 u1 处于 Install Fail，初始化失败，建议删除重建。", reply,
		"flag-on + guards-passed returns the skill loop's final answer, not a Go-chain DiagResult JSON")
	assert.Equal(t, []string{"DescribeCompShareInstance"}, exec.calls,
		"the skill loop drove exactly the read tool the model chose")
	require.GreaterOrEqual(t, mock.callIdx, 2, "the body-driven loop made its own LLM calls")
}
