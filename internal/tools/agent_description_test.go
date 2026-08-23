package tools

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestWorkflowAgentDescriptionsContainOnlyOperationBoundaries(t *testing.T) {
	registry := DefaultCapabilityRegistry()
	for _, operation := range []string{
		"CreateInstanceWorkflow", "StopInstanceWorkflow", "StartInstanceWorkflow", "RebootInstanceWorkflow",
		"RenameInstanceWorkflow", "ResetPasswordWorkflow", "SetStopSchedulerWorkflow", "CancelStopSchedulerWorkflow",
		"UpdateInstancePortsWorkflow",
		"ResizeInstanceWorkflow", "ResizeDiskWorkflow", "ReinstallInstanceWorkflow", "CreateDiskWorkflow",
		"CreateCustomImageWorkflow", "CloneCustomImageWorkflow", "EnableNetOptimizerWorkflow", "CreateCFSWorkflow", "ResizeCFSWorkflow",
	} {
		capability, ok := registry.Lookup(operation)
		require.True(t, ok, operation)
		description := WorkflowAgentDescription(capability.AgentInstruction)
		require.Contains(t, description, "调用/边界：", operation)
		require.Contains(t, description, capability.AgentInstruction, operation)
		for _, duplicate := range []string{"接续：", "失败：", "输入示例"} {
			require.NotContains(t, description, duplicate, operation)
		}
	}
}

func TestWorkflowAgentDescriptionFailsClosedWithoutABoundary(t *testing.T) {
	description := WorkflowAgentDescription("")
	require.Contains(t, description, "仅在用户明确要求执行该操作时调用")
}
