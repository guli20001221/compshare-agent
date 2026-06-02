package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/compshare-agent/internal/knowledge"
	"github.com/compshare-agent/internal/llm"
	"github.com/compshare-agent/internal/observability"
	"github.com/compshare-agent/internal/tools"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	openai "github.com/sashabaranov/go-openai"
)

// These tests pin the P2a routing fork: with USE_SKILL_EXECUTOR on, the piloted
// DiagnosePortOrFirewall action runs through the body-driven orchestrator loop
// (the LLM drives read tools); with it off (shipped default), the deterministic
// Go chain runs and never touches the LLM. The flag is the only difference.

func TestExecuteDiagnosis_FlagOn_RoutesThroughSkillExecutor(t *testing.T) {
	prev := SkillExecutorEnabled()
	SetSkillExecutorEnabled(true)
	defer SetSkillExecutorEnabled(prev)
	prevPilots := SkillExecutorDiagnosisPilots()
	SetSkillExecutorDiagnosisPilots([]string{"diagnose_port_firewall"})
	defer SetSkillExecutorDiagnosisPilots(prevPilots)

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

func TestExecuteDiagnosis_PortFirewallProbesKnowledgeButDoesNotInjectChunkContent(t *testing.T) {
	prev := SkillExecutorEnabled()
	SetSkillExecutorEnabled(true)
	defer SetSkillExecutorEnabled(prev)
	prevPilots := SkillExecutorDiagnosisPilots()
	SetSkillExecutorDiagnosisPilots([]string{"diagnose_port_firewall"})
	defer SetSkillExecutorDiagnosisPilots(prevPilots)

	chunk := knowledge.KBChunk{
		ChunkID:     "runbook-port-001",
		KBVersion:   "kb.test",
		SourceType:  "runbook",
		ProductArea: "network",
		ACL:         "customer_safe",
		Confidence:  "high",
		Title:       "Service port reachability",
		Content:     "For service ports, first verify the instance is Running, then compare exposed software ports.",
	}
	retriever := &scriptedKnowledgeRetriever{results: []knowledge.RetrievalResult{{
		Enabled:   true,
		KBVersion: "kb.test",
		Hits:      []knowledge.KBChunk{chunk},
		HitItems:  []knowledge.RetrievalHit{{Chunk: chunk, Score: 0.95, Kept: true}},
	}}}
	exec := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {"UHostSet": []any{map[string]any{
			"UHostId": "u1", "State": "Running",
		}}},
	}}
	mock := &mockLLM{responses: []llm.ChatResponse{
		{Content: `{"action":"DescribeCompShareInstance","args":{"UHostIds":["u1"]}}`},
		{Content: `{"final":"先按知识库核对实例运行状态，再看端口配置。"}`},
	}}
	eng := NewWithDeps(mock, exec, nil)
	eng.SetKnowledgeRetriever(retriever)
	eng.Init(context.Background())
	exec.calls = nil
	eng.lastUserMsg = "webui 的端口打不开，是不是被防火墙挡了"

	var events []StepEvent
	reply := eng.executeDiagnosis(context.Background(), "DiagnosePortOrFirewall",
		map[string]any{"UHostId": "u1", "Service": "webui"}, func(ev StepEvent) {
			events = append(events, ev)
		})

	// Regression guard for the citation/leakage hole (#207): port diagnosis MAY
	// probe the KB for observability (SearchKnowledge step + RetrievalTrace), and the
	// probe runs before the live read-only tools, but it must NOT place any retrieved
	// chunk content into the diagnosis model's prompt. A final diagnosis answer that
	// consumed KB text would bypass the route-dependent cited-guard. When a
	// citation-aware evidence adapter lands, update this test alongside it.
	assert.NotEmpty(t, reply, "diagnosis still produces an answer")
	require.Len(t, retriever.calls, 1, "KB is probed exactly once (observability)")
	assert.Equal(t, eng.lastUserMsg, retriever.calls[0].question)
	assert.Equal(t, []string{"DescribeCompShareInstance"}, exec.calls)
	assertStepOrder(t, events, "SearchKnowledge", "DescribeCompShareInstance")
	require.NotEmpty(t, mock.calls)
	firstPrompt := joinedMessages(mock.calls[0].Messages)
	assert.NotContains(t, firstPrompt, "KnowledgeEvidence",
		"retrieved evidence must not be injected into the diagnosis model prompt")
	assert.NotContains(t, firstPrompt, "Service port reachability",
		"retrieved chunk title must not reach the diagnosis model")
	assert.NotContains(t, firstPrompt, "For service ports, first verify",
		"retrieved chunk content must not reach the diagnosis model")
}

func TestExecuteDiagnosis_RAGAsEvidenceDoesNotApplyToSSH(t *testing.T) {
	prev := SkillExecutorEnabled()
	SetSkillExecutorEnabled(true)
	defer SetSkillExecutorEnabled(prev)
	prevPilots := SkillExecutorDiagnosisPilots()
	SetSkillExecutorDiagnosisPilots([]string{"diagnose_ssh"})
	defer SetSkillExecutorDiagnosisPilots(prevPilots)

	chunk := knowledge.KBChunk{ChunkID: "ssh-001", KBVersion: "kb.test", Title: "SSH", Content: "SSH docs"}
	retriever := &scriptedKnowledgeRetriever{results: []knowledge.RetrievalResult{{
		Enabled: true,
		Hits:    []knowledge.KBChunk{chunk},
	}}}
	exec := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {"UHostSet": []any{map[string]any{
			"UHostId": "u1", "State": "Running",
		}}},
	}}
	mock := &mockLLM{responses: []llm.ChatResponse{
		{Content: `{"action":"DescribeCompShareInstance","args":{"UHostIds":["u1"]}}`},
		{Content: `{"final":"SSH 排查完成。"}`},
	}}
	eng := NewWithDeps(mock, exec, nil)
	eng.SetKnowledgeRetriever(retriever)
	eng.Init(context.Background())
	exec.calls = nil
	eng.lastUserMsg = "ssh permission denied 进不去"

	var events []StepEvent
	reply := eng.executeDiagnosis(context.Background(), "DiagnoseSSH",
		map[string]any{"UHostId": "u1"}, func(ev StepEvent) {
			events = append(events, ev)
		})

	assert.Contains(t, reply, "SSH")
	assert.Empty(t, retriever.calls)
	for _, ev := range events {
		assert.NotEqual(t, "SearchKnowledge", ev.Action)
	}
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

func assertStepOrder(t *testing.T, events []StepEvent, first, second string) {
	t.Helper()
	firstIdx, secondIdx := -1, -1
	for i, ev := range events {
		if ev.Action == first && firstIdx == -1 {
			firstIdx = i
		}
		if ev.Action == second && secondIdx == -1 {
			secondIdx = i
		}
	}
	require.NotEqualf(t, -1, firstIdx, "missing step %s in %#v", first, events)
	require.NotEqualf(t, -1, secondIdx, "missing step %s in %#v", second, events)
	assert.Less(t, firstIdx, secondIdx, "%s must occur before %s", first, second)
}

func joinedMessages(messages []openai.ChatCompletionMessage) string {
	var parts []string
	for _, msg := range messages {
		parts = append(parts, msg.Content)
	}
	return strings.Join(parts, "\n")
}

func TestExecuteDiagnosis_FlagOn_SkillExecutorFailureFallsBackToGoChain(t *testing.T) {
	prev := SkillExecutorEnabled()
	SetSkillExecutorEnabled(true)
	defer SetSkillExecutorEnabled(prev)
	prevPilots := SkillExecutorDiagnosisPilots()
	SetSkillExecutorDiagnosisPilots([]string{"diagnose_port_firewall"})
	defer SetSkillExecutorDiagnosisPilots(prevPilots)

	exec := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance":     {"UHostSet": []any{map[string]any{"UHostId": "u1", "State": "Running"}}},
		"DescribeCompShareSoftwarePort": {"SoftwarePort": []any{map[string]any{"Software": "JupyterLab", "Port": float64(8888)}}},
	}}
	mock := &mockLLM{responses: []llm.ChatResponse{
		{Content: `not json`},
		{Content: `still not json`},
	}}
	eng := NewWithDeps(mock, exec, nil)
	eng.Init(context.Background())
	exec.calls = nil

	var events []StepEvent
	reply := eng.executeDiagnosis(context.Background(), "DiagnosePortOrFirewall",
		map[string]any{"UHostId": "u1", "Service": "JupyterLab"}, func(ev StepEvent) {
			events = append(events, ev)
		})

	assert.Contains(t, reply, `"success":true`,
		"skill executor unrecovered errors must fall back to the deterministic Go-chain result")
	assert.Equal(t, []string{"DescribeCompShareInstance", "DescribeCompShareSoftwarePort"}, exec.calls,
		"after the skill loop safe-fails, the shipped Go diagnosis chain must still run")
	require.Equal(t, 2, mock.callIdx, "the body-driven loop should fail after its malformed retry budget")

	var sawSkillExecutorError bool
	for _, ev := range events {
		if ev.Type == StepError &&
			ev.Action == "DiagnosePortOrFirewall" &&
			ev.Source == observability.ToolSourceDiagnosisInternal {
			sawSkillExecutorError = true
			assert.Contains(t, ev.Message, "skill executor: unrecovered")
		}
	}
	assert.True(t, sawSkillExecutorError, "the failed skill loop must still be visible in trace events")
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

func TestPilotedDiagnosisSkillsDeclareOnlyReadOnlyTools(t *testing.T) {
	policies := tools.DefaultToolExecutionPolicies()
	for _, skillName := range KnownDiagnosisSkillExecutorPilots() {
		skill, ok := findGeneratedSkill(skillName)
		require.Truef(t, ok, "piloted skill %q must exist in generated registry", skillName)
		require.NotEmptyf(t, skill.RequiredTools, "piloted skill %q must declare required tools", skillName)
		for _, toolName := range skill.RequiredTools {
			policy, ok := policies[toolName]
			require.Truef(t, ok, "tool %q used by %q has no execution policy", toolName, skillName)
			assert.NotEqualf(t, tools.ActionRouteWorkflow, policy.Route,
				"diagnosis skill %q must not expose workflow tool %q", skillName, toolName)
			assert.NotEqualf(t, tools.ActionClassMutating, policy.Class,
				"diagnosis skill %q must not expose mutating tool %q", skillName, toolName)
			assert.NotEqualf(t, tools.ActionClassDestructive, policy.Class,
				"diagnosis skill %q must not expose destructive tool %q", skillName, toolName)
		}
	}
}

func TestDiagnosisSkillExecutorPilotForAction_RequiresExplicitAllowlist(t *testing.T) {
	prev := SkillExecutorEnabled()
	SetSkillExecutorEnabled(true)
	defer SetSkillExecutorEnabled(prev)
	prevPilots := SkillExecutorDiagnosisPilots()
	defer SetSkillExecutorDiagnosisPilots(prevPilots)

	SetSkillExecutorDiagnosisPilots(nil)
	got, ok := diagnosisSkillExecutorPilotForAction("DiagnosePortOrFirewall")
	assert.False(t, ok, "global flag alone must not pilot diagnosis skills")
	assert.Empty(t, got)

	SetSkillExecutorDiagnosisPilots([]string{"diagnose_port_firewall"})
	got, ok = diagnosisSkillExecutorPilotForAction("DiagnosePortOrFirewall")
	assert.True(t, ok)
	assert.Equal(t, "diagnose_port_firewall", got)

	got, ok = diagnosisSkillExecutorPilotForAction("DiagnoseSSH")
	assert.False(t, ok, "only explicitly allowlisted diagnosis skills may pilot")
	assert.Empty(t, got)
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
	prevPilots := SkillExecutorDiagnosisPilots()
	SetSkillExecutorDiagnosisPilots([]string{"diagnose_init_failure"})
	defer SetSkillExecutorDiagnosisPilots(prevPilots)

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
	prevPilots := SkillExecutorDiagnosisPilots()
	SetSkillExecutorDiagnosisPilots([]string{"diagnose_init_failure"})
	defer SetSkillExecutorDiagnosisPilots(prevPilots)

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
