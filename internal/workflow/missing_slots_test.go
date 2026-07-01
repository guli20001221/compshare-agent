package workflow

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestMissingSlotsForFailureMapsWorkflowOwnedMessages(t *testing.T) {
	tests := []struct {
		name     string
		workflow string
		message  string
		want     []string
	}{
		{
			name:     "create disk size",
			workflow: "CreateDiskWorkflow",
			message:  "步骤「查询实例」参数构建失败: " + createDiskMissingSizeMessage,
			want:     []string{"size_gb"},
		},
		{
			name:     "resize disk target size",
			workflow: "ResizeDiskWorkflow",
			message:  "步骤「查询实例」参数构建失败: " + resizeDiskMissingTargetMessage,
			want:     []string{"target_size_gb"},
		},
		{
			name:     "resize instance spec",
			workflow: "ResizeInstanceWorkflow",
			message:  "步骤「查询实例」参数构建失败: " + resizeInstanceMissingSpecMessage,
			want:     []string{"cpu", "memory_gb", "gpu_count"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, MissingSlotsForFailure(tc.workflow, tc.message))
		})
	}
}

func TestMissingSlotsForFailureIgnoresUnknownMessages(t *testing.T) {
	assert.Nil(t, MissingSlotsForFailure("CreateDiskWorkflow", "未找到该实例。"))
	assert.Nil(t, MissingSlotsForFailure("UnknownWorkflow", "步骤「查询实例」参数构建失败: "+createDiskMissingSizeMessage))
}
