package engine

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The Agent proposes a write through a per-operation Request<Operation> tool; the
// legacy ProposeAction_<Operation> alias was retired. This pins the first hop a
// create turn takes (RequestCreateInstance) and guards the alias from returning
// to the advertised window or the tool->operation mapping.
func TestProposalToolFirstHopIsRequestNotLegacyAlias(t *testing.T) {
	names := centralAgentToolNames(true)
	require.Contains(t, names, "RequestCreateInstance",
		"the create proposal tool the Agent calls first must be RequestCreateInstance")
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
