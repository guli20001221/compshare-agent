package tools

import (
	"testing"

	"github.com/compshare-agent/internal/security"
	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/require"
)

func TestDefaultCapabilityRegistryOwnsEveryAgentToolAndPolicy(t *testing.T) {
	registry := DefaultCapabilityRegistry()
	require.NoError(t, registry.ValidateSafety())

	exposed := 0
	for _, capability := range registry.All() {
		if !capability.ExposedToAgent || capability.Stage != CapabilityStageActive {
			continue
		}
		exposed++
		require.NotNil(t, capability.Tool.Function)
		require.Equal(t, capability.Name, capability.Tool.Function.Name)
		require.Equal(t, capability.Tool.Function.Description, capability.AgentInstruction)
		require.Equal(t, capability.Name, capability.Policy.Action)
	}
	require.Equal(t, len(Registry), exposed)
	_, ok := registry.Lookup("ReadPlatformCapability")
	require.False(t, ok, "the generic capability+slots adapter must not remain executable")
	for _, tool := range registry.VisibleTools(ToolScope{Mode: ToolScopeReadOnlyFull}, true) {
		require.NotEqual(t, "ReadPlatformCapability", tool.Function.Name)
	}

	for name, policy := range buildToolExecutionPolicies() {
		capability, ok := registry.Lookup(name)
		require.Truef(t, ok, "missing capability %s", name)
		require.Equal(t, policy, capability.Policy)
	}
}

func TestCapabilityRegistryDerivesResultOwnershipFromExecutionRoute(t *testing.T) {
	registry := DefaultCapabilityRegistry()
	cases := []struct {
		name     string
		contract ResultContract
		owner    string
	}{
		// SearchKnowledge is now the only ActionRouteKnowledge member; the GetGPUSpecs
		// row that used to sit beside it went with the static GPU table.
		{name: "SearchKnowledge", contract: ResultContractGroundedAnswer, owner: "grounding"},
		{name: "DescribeCompShareInstance", contract: ResultContractModelObservation, owner: "agent"},
		{name: "StopInstanceWorkflow", contract: ResultContractWorkflowResult, owner: "workflow"},
	}
	for _, tc := range cases {
		capability, ok := registry.Lookup(tc.name)
		require.True(t, ok)
		require.Equal(t, tc.contract, capability.ResultContract)
		require.Equal(t, tc.owner, capability.ResultOwner)
	}
}

func TestCapabilityRegistryPreservesDenyByDefaultAndReadOnlyWindows(t *testing.T) {
	registry := DefaultCapabilityRegistry()
	require.Empty(t, registry.VisibleTools(ToolScope{Mode: ToolScopeNamed}, true))
	require.Empty(t, registry.VisibleTools(ToolScope{Mode: ToolScopeNamed, Names: []string{"does-not-exist"}}, true))

	for _, tool := range registry.VisibleTools(ToolScope{}, true) {
		capability, ok := registry.Lookup(tool.Function.Name)
		require.True(t, ok)
		require.NotEqual(t, ActionClassMutating, capability.Policy.Class)
		require.NotEqual(t, ActionRouteWorkflow, capability.Policy.Route)
	}

	named := registry.VisibleTools(ToolScope{Mode: ToolScopeNamed, Names: []string{"SearchKnowledge", "StopInstanceWorkflow"}}, false)
	require.Equal(t, []string{"SearchKnowledge"}, toolNames(named))
}

func TestCapabilityRegistryRejectsMissingPolicyAndUnsafeL1(t *testing.T) {
	_, err := BuildCapabilityRegistry([]openai.Tool{{Type: openai.ToolTypeFunction, Function: &openai.FunctionDefinition{Name: "orphan"}}}, nil)
	require.ErrorContains(t, err, "no execution policy")

	registry, err := BuildCapabilityRegistry(nil, map[string]ToolExecutionPolicy{
		"unsafe": {Action: "unsafe", SecurityLevel: security.L1, Class: ActionClassMutating, NeedsConfirm: false},
	})
	require.NoError(t, err)
	require.ErrorContains(t, registry.ValidateSafety(), "does not require confirmation")
}

func toolNames(in []openai.Tool) []string {
	out := make([]string, 0, len(in))
	for _, tool := range in {
		if tool.Function != nil {
			out = append(out, tool.Function.Name)
		}
	}
	return out
}
