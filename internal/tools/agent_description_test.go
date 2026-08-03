package tools

import (
	"encoding/json"
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
		guidedIntake := operation == "CreateInstanceWorkflow"
		description := WorkflowAgentDescription(operation, capability.AgentInstruction, guidedIntake)
		for _, section := range []string{"调用/边界：", "接续：", "失败："} {
			require.Contains(t, description, section, operation)
		}
		require.Contains(t, description, "提交后缺项", operation)
		if guidedIntake {
			require.Contains(t, description, "缺项→引导卡", operation)
		} else {
			require.Contains(t, description, "missing_fields", operation)
			require.NotContains(t, description, "缺项→引导卡", operation)
		}
		if _, complex := workflowInputExamples[operation]; complex {
			require.Contains(t, description, "输入示例", operation)
		}
	}
}

func TestWorkflowAgentDescriptionDoesNotNeedWorkflowStepNames(t *testing.T) {
	description := WorkflowAgentDescription("CreateCustomImageWorkflow", "从实例发起自制镜像制作。", false)
	require.NotContains(t, description, "CreateCompShareCustomImage")
	require.NotContains(t, description, "DescribeCompShare")
	require.Contains(t, description, "输入示例")
}

func TestWorkflowInputExamplesMatchCapabilitySchemas(t *testing.T) {
	registry := DefaultCapabilityRegistry()
	for operation, raw := range workflowInputExamples {
		var input map[string]any
		require.NoError(t, json.Unmarshal([]byte(raw), &input), operation)
		capability, ok := registry.Lookup(operation)
		require.True(t, ok, operation)
		parameters, ok := capability.Tool.Function.Parameters.(map[string]any)
		require.True(t, ok, operation)
		properties, ok := parameters["properties"].(map[string]any)
		require.True(t, ok, operation)
		for field, value := range input {
			property, ok := properties[field].(map[string]any)
			require.True(t, ok, "%s.%s", operation, field)
			if property["type"] == "string" {
				_, ok := value.(string)
				require.True(t, ok, "%s.%s must remain a string example", operation, field)
			}
			if enum, ok := property["enum"].([]any); ok {
				require.Contains(t, enum, value, "%s.%s must remain an allowed enum value", operation, field)
			}
		}
	}
}
