package engine

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestProposalToolExposureAndMapping asserts ONLY that the catalog exposes the
// per-operation Request<Operation> tool and maps it back to its operation, and
// that the retired ProposeAction_<Operation> alias is gone. It deliberately does
// NOT assert that the real model actually calls RequestCreateInstance first on a
// create turn: under the free ReAct loop that is a probabilistic model-behavior
// property to be measured from real-model traces, not guaranteed by the tool list.
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

// The single model-visible proposal contract is authored once
// (proposalInvocationContract{Prefix,Suffix}) and shared by every
// Request<Operation> tool, so the per-op description cannot drift from a second
// copy — the base ProposeAction template deliberately no longer restates it.
func TestRequestToolDescriptionUsesSingleContractSource(t *testing.T) {
	var desc string
	for _, tool := range centralAgentToolWindow(true) {
		if tool.Function != nil && tool.Function.Name == "RequestCreateInstance" {
			desc = tool.Function.Description
			break
		}
	}
	require.NotEmpty(t, desc, "RequestCreateInstance must be advertised when mutating is enabled")
	require.True(t, strings.HasPrefix(desc, proposalInvocationContractPrefix),
		"the per-op description must open with the single contract preamble")
	require.True(t, strings.HasSuffix(desc, proposalInvocationContractSuffix),
		"the per-op description must close with the single contract suffix")
}
