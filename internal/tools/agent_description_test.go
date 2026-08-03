package tools

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestWorkflowAgentDescriptionsUseOneInteractionTemplate(t *testing.T) {
	registry := DefaultCapabilityRegistry()
	for _, operation := range []string{
		"CreateInstanceWorkflow", "StopInstanceWorkflow", "StartInstanceWorkflow", "RebootInstanceWorkflow",
		"RenameInstanceWorkflow", "ResetPasswordWorkflow", "SetStopSchedulerWorkflow", "CancelStopSchedulerWorkflow",
		"ResizeInstanceWorkflow", "ReinstallInstanceWorkflow", "CreateDiskWorkflow", "ResizeDiskWorkflow",
		"CreateCustomImageWorkflow", "CloneCustomImageWorkflow", "EnableNetOptimizerWorkflow", "CreateCFSWorkflow", "ResizeCFSWorkflow",
	} {
		capability, ok := registry.Lookup(operation)
		require.True(t, ok, operation)
		description := WorkflowAgentDescription(operation, capability.AgentInstruction)
		for _, section := range []string{"何时调用：", "不会做什么：", "卡片如何接续：", "失败后下一步："} {
			require.Contains(t, description, section, operation)
		}
		if _, complex := workflowInputExamples[operation]; complex {
			require.Contains(t, description, "输入示例", operation)
		}
	}
}

func TestWorkflowAgentDescriptionDoesNotNeedWorkflowStepNames(t *testing.T) {
	description := WorkflowAgentDescription("CreateCustomImageWorkflow", "从实例发起自制镜像制作。")
	require.NotContains(t, description, "CreateCompShareCustomImage")
	require.NotContains(t, description, "DescribeCompShare")
	require.Contains(t, description, "输入示例")
}
