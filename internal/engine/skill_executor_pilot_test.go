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
