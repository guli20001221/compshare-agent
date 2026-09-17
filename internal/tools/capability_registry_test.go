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
		if !capability.ExposedToAgent {
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

	for name, policy := range buildToolExecutionPolicies() {
		capability, ok := registry.Lookup(name)
		require.Truef(t, ok, "missing capability %s", name)
		require.Equal(t, policy, capability.Policy)
	}
}

func TestCapabilityRegistryRejectsMissingPolicyAndUnsafeL1(t *testing.T) {
	_, err := BuildCapabilityRegistryFromDefinitions([]CapabilityDefinition{{Tool: openai.Tool{Type: openai.ToolTypeFunction, Function: &openai.FunctionDefinition{Name: "orphan"}}, ExposedToAgent: true}}, nil)
	require.ErrorContains(t, err, "no execution policy")

	registry, err := BuildCapabilityRegistryFromDefinitions(nil, map[string]ToolExecutionPolicy{
		"unsafe": {Action: "unsafe", SecurityLevel: security.L1, Class: ActionClassMutating, NeedsConfirm: false},
	})
	require.NoError(t, err)
	require.ErrorContains(t, registry.ValidateSafety(), "does not require confirmation")
}
