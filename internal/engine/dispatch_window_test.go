package engine

import (
	"strings"
	"testing"

	"github.com/compshare-agent/internal/tools"
	"github.com/stretchr/testify/require"
)

// TestProposalToolExposureAndMapping asserts ONLY that the catalog exposes the
// per-operation Request<Operation> tool and maps it back to its operation, and
// that the retired ProposeAction_<Operation> alias is gone. It does NOT — and its
// old name "FirstHopIsRequest" misleadingly implied it did — assert that the real
// model actually calls RequestCreateInstance first on a create turn: under the free
// ReAct loop that is a probabilistic model-behavior property, to be measured from
// real-model traces for acceptance, not something a tool-list test can prove.
func TestProposalToolExposureAndMapping(t *testing.T) {
	names := centralAgentToolNames(true)
	require.Contains(t, names, "RequestCreateInstance",
		"the create proposal tool must be advertised as RequestCreateInstance")
	require.NotContains(t, names, "ProposeAction_CreateInstanceWorkflow",
		"the retired ProposeAction_* alias must not be advertised to the model")

	op, ok := proposalOperationForTool("RequestCreateInstance")
	require.True(t, ok)
	require.Equal(t, "CreateInstanceWorkflow", op)

	_, ok = proposalOperationForTool("ProposeAction_CreateInstanceWorkflow")
	require.False(t, ok, "the retired alias must no longer resolve to an operation")
}

// The system prompt owns the shared write-proposal behavior. Request tools carry
// only their operation-specific semantic boundary, sourced from the capability
// registry rather than the workflow's internal execution-step description.
func TestRequestToolDescriptionUsesCapabilityBoundaryNotWorkflowSteps(t *testing.T) {
	var desc string
	for _, tool := range centralAgentToolWindow(true) {
		if tool.Function != nil && tool.Function.Name == "RequestCreateInstance" {
			desc = tool.Function.Description
			break
		}
	}
	require.NotEmpty(t, desc, "RequestCreateInstance must be advertised when mutating is enabled")
	capability, ok := tools.DefaultCapabilityRegistry().Lookup("CreateInstanceWorkflow")
	require.True(t, ok)
	require.Equal(t, capability.AgentInstruction, desc)
	require.NotContains(t, desc, "→", "workflow execution steps are runtime-only")
	require.NotContains(t, desc, "参数可不完整", "shared proposal behavior belongs only in the system prompt")
	require.NotContains(t, desc, "本工具不直接执行", "shared proposal behavior belongs only in the system prompt")
}

func TestRequestToolDescriptionsDoNotRepeatSharedPromptOrExecutionChains(t *testing.T) {
	for _, tool := range centralAgentToolWindow(true) {
		if tool.Function == nil || !strings.HasPrefix(tool.Function.Name, "Request") {
			continue
		}
		desc := tool.Function.Description
		require.NotEmpty(t, desc, tool.Function.Name)
		require.NotContains(t, desc, "参数可不完整", tool.Function.Name)
		require.NotContains(t, desc, "服务端负责缺失字段", tool.Function.Name)
		require.NotContains(t, desc, "本工具不直接执行", tool.Function.Name)
		require.NotContains(t, desc, "→", tool.Function.Name)
		require.NotContains(t, desc, "->", tool.Function.Name)
		require.NotContains(t, desc, "自动执行", tool.Function.Name)
		require.NotContains(t, desc, "确认式工作流", tool.Function.Name)
		for _, internalAPI := range []string{"DescribeCompShare", "GetCompShare", "CreateCompShare", "SyncCompShare"} {
			require.NotContains(t, desc, internalAPI, tool.Function.Name)
		}
	}
}

func TestSensitiveRequestToolExplainsServerSideSecretInjection(t *testing.T) {
	var description string
	var parameters any
	for _, tool := range centralAgentToolWindow(true) {
		if tool.Function != nil && tool.Function.Name == "RequestResetPassword" {
			description = tool.Function.Description
			parameters = tool.Function.Parameters
			break
		}
	}
	require.NotEmpty(t, description)
	require.Contains(t, description, "敏感值已由服务端安全接收")
	properties := parameters.(map[string]any)["properties"].(map[string]any)
	require.NotContains(t, properties, "Password")
}
