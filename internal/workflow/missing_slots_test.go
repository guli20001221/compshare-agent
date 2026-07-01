package workflow

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

func TestMissingSlotsFromErrorDoesNotDependOnMessageText(t *testing.T) {
	err := NewMissingSlotError("任意用户提示文案，后续可以安全改写", "size_gb")

	assert.Equal(t, []string{"size_gb"}, MissingSlotsFromError(err))
}

func TestWorkflowMissingSlotsAreStructured(t *testing.T) {
	tests := []struct {
		name string
		def  *Definition
		args map[string]any
		want []string
	}{
		{
			name: "create disk missing size",
			def:  CreateDiskDef(),
			args: map[string]any{"UHostId": "uhost-1"},
			want: []string{"size_gb"},
		},
		{
			name: "resize disk missing target size",
			def:  ResizeDiskDef(),
			args: map[string]any{"UHostId": "uhost-1", "DiskType": "boot"},
			want: []string{"target_size_gb"},
		},
		{
			name: "resize instance missing spec",
			def:  ResizeInstanceDef(),
			args: map[string]any{"UHostId": "uhost-1"},
			want: []string{"cpu", "memory_gb", "gpu_count"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			eng := NewEngine(&mockExecutor{}, nil, nil)
			result, err := eng.Run(context.Background(), tc.def, tc.args)

			require.NoError(t, err)
			require.False(t, result.Success)
			assert.Equal(t, tc.want, result.MissingSlots)
		})
	}
}

func TestTaskSlotSpecsParseUpdatesAndBuildArgs(t *testing.T) {
	updates := TaskSlotUpdatesFromUserText("CreateDiskWorkflow", []string{"size_gb"}, "200G")
	assert.Equal(t, map[string]string{"size_gb": "200G"}, updates)

	args, missing := TaskArgsFromSlots("CreateDiskWorkflow", map[string]string{
		"instance_id": "uhost-1",
		"size_gb":     "200G",
	})
	require.Empty(t, missing)
	assert.Equal(t, map[string]any{"UHostId": "uhost-1", "Size": float64(200)}, args)

	updates = TaskSlotUpdatesFromUserText("ResizeDiskWorkflow", []string{"target_size_gb"}, "300GB")
	assert.Equal(t, map[string]string{"target_size_gb": "300GB"}, updates)

	updates = TaskSlotUpdatesFromUserText("ResizeInstanceWorkflow", []string{"cpu", "memory_gb", "gpu_count"}, "4C8G 1张卡")
	assert.Equal(t, map[string]string{"cpu": "4", "memory_gb": "8192", "gpu_count": "1"}, updates)

	args, missing = TaskArgsFromSlots("ResizeInstanceWorkflow", map[string]string{
		"instance_id": "uhost-1",
		"cpu":         "4",
		"memory_gb":   "8192",
		"gpu_count":   "1",
	})
	require.Empty(t, missing)
	assert.Equal(t, map[string]any{"UHostId": "uhost-1", "Cpu": float64(4), "Memory": float64(8192), "Gpu": float64(1)}, args)
}
