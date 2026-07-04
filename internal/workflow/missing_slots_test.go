package workflow

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
		{
			name: "scheduler missing time",
			def:  SetStopSchedulerDef(),
			args: map[string]any{"UHostId": "uhost-1"},
			want: []string{"stop_time"},
		},
		{
			name: "reinstall missing image",
			def:  ReinstallInstanceDef(),
			args: map[string]any{"UHostId": "uhost-1"},
			want: []string{"image_id"},
		},
		{
			name: "rename missing name",
			def:  RenameInstanceDef(),
			args: map[string]any{"UHostId": "uhost-1"},
			want: []string{"name"},
		},
		{
			name: "create custom image missing name",
			def:  CreateCustomImageDef(),
			args: map[string]any{"UHostId": "uhost-1"},
			want: []string{"name"},
		},
		{
			name: "create cfs missing name",
			def:  CreateCFSDef(),
			args: map[string]any{"Size": float64(100), "Zone": "cn-pod-01"},
			want: []string{"name"},
		},
		{
			name: "resize cfs missing target size",
			def:  ResizeCFSDef(),
			args: map[string]any{"CfsId": "cfs-1"},
			want: []string{"target_size_gb"},
		},
		{
			name: "net optimizer missing zone",
			def:  EnableNetOptimizerDef(),
			args: map[string]any{},
			want: []string{"zone"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			describeResult := stoppedInstanceResult()
			if tc.name == "scheduler missing time" {
				describeResult = map[string]any{"UHostSet": []any{map[string]any{
					"UHostId":    "uhost-1",
					"Name":       "running",
					"State":      "Running",
					"Region":     "cn-wlcb",
					"Zone":       "cn-wlcb-01",
					"ChargeType": "Dynamic",
				}}}
			}
			eng := NewEngine(&mockExecutor{results: map[string]map[string]any{
				"DescribeCompShareInstance":    describeResult,
				"DescribeCompShareSupportZone": cfsSupportZone("cn-pod-01", "cn-pod", "容器一区", 9001, 3001, true),
				"DescribeCFS":                  cfsDescribeResult(),
			}}, nil, nil)
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
	updates = TaskSlotUpdatesFromUserText("CreateDiskWorkflow", []string{"size_gb"}, "给这台加一块 200G 数据盘")
	assert.Equal(t, map[string]string{"size_gb": "200G"}, updates)
	updates = TaskSlotUpdatesFromUserText("CreateDiskWorkflow", []string{"size_gb"}, "当前 100G，再加一块 200G")
	assert.Nil(t, updates)

	args, missing := TaskArgsFromSlots("CreateDiskWorkflow", map[string]string{
		"instance_id": "uhost-1",
		"size_gb":     "200G",
	})
	require.Empty(t, missing)
	assert.Equal(t, map[string]any{"UHostId": "uhost-1", "Size": float64(200)}, args)

	updates = TaskSlotUpdatesFromUserText("ResizeDiskWorkflow", []string{"target_size_gb"}, "300GB")
	assert.Equal(t, map[string]string{"target_size_gb": "300GB"}, updates)
	updates = TaskSlotUpdatesFromUserText("ResizeDiskWorkflow", []string{"target_size_gb"}, "把系统盘扩到 300GB")
	assert.Equal(t, map[string]string{"target_size_gb": "300GB"}, updates)
	updates = TaskSlotUpdatesFromUserText("ResizeDiskWorkflow", []string{"target_size_gb"}, "从 100G 扩到 300GB")
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

	updates = TaskSlotUpdatesFromUserText("SetStopSchedulerWorkflow", []string{"stop_time"}, "30分钟后")
	assert.Equal(t, map[string]string{"stop_time": "30分钟后"}, updates)
	args, missing = TaskArgsFromSlots("SetStopSchedulerWorkflow", map[string]string{
		"instance_id": "uhost-1",
		"stop_time":   "30分钟后",
	})
	require.Empty(t, missing)
	assert.Equal(t, map[string]any{"UHostId": "uhost-1", "AfterMinutes": float64(30)}, args)

	updates = TaskSlotUpdatesFromUserText("ResizeCFSWorkflow", []string{"target_size_gb"}, "200GB")
	assert.Equal(t, map[string]string{"target_size_gb": "200GB"}, updates)
	updates = TaskSlotUpdatesFromUserText("ResizeCFSWorkflow", []string{"target_size_gb"}, "这个 CFS 扩容到 200G")
	assert.Equal(t, map[string]string{"target_size_gb": "200G"}, updates)
	updates = TaskSlotUpdatesFromUserText("ResizeCFSWorkflow", []string{"target_size_gb"}, "从 100G 扩到 200G")
	assert.Equal(t, map[string]string{"target_size_gb": "200G"}, updates)
	args, missing = TaskArgsFromSlots("ResizeCFSWorkflow", map[string]string{
		"cfs_id":         "cfs-abc123",
		"target_size_gb": "200GB",
	})
	require.Empty(t, missing)
	assert.Equal(t, map[string]any{"CfsId": "cfs-abc123", "Size": float64(200)}, args)

	updates = TaskSlotUpdatesFromUserText("CreateCFSWorkflow", []string{"name"}, "codex-cfs-test")
	assert.Equal(t, map[string]string{"name": "codex-cfs-test"}, updates)
	updates = TaskSlotUpdatesFromUserText("CreateCFSWorkflow", []string{"zone"}, "华北二A")
	assert.Equal(t, map[string]string{"zone": "华北二A"}, updates)
	updates = TaskSlotUpdatesFromUserText("CreateCFSWorkflow", []string{"name", "zone"}, "华北二A")
	assert.Nil(t, updates, "multi-slot CFS follow-up must be decided by the context model, not guessed by fallback parsing")
	args, missing = TaskArgsFromSlots("CreateCFSWorkflow", map[string]string{
		"name":    "codex-cfs-test",
		"size_gb": "100G",
		"zone":    "cn-pod-01",
	})
	require.Empty(t, missing)
	assert.Equal(t, map[string]any{"Name": "codex-cfs-test", "Size": float64(100), "Zone": "cn-pod-01"}, args)

	args, missing = TaskArgsFromSlots("EnableNetOptimizerWorkflow", map[string]string{"zone": "cn-pod-01"})
	require.Empty(t, missing)
	assert.Equal(t, map[string]any{"Zone": "cn-pod-01"}, args)

	args, missing = TaskArgsFromSlots("CreateCustomImageWorkflow", map[string]string{
		"instance_id": "uhost-1",
		"name":        "snapshot-v1",
		"description": "training environment",
	})
	require.Empty(t, missing)
	assert.Equal(t, map[string]any{"UHostId": "uhost-1", "Name": "snapshot-v1", "Description": "training environment"}, args)

	args, missing = TaskArgsFromSlots("RenameInstanceWorkflow", map[string]string{
		"instance_id": "uhost-1",
		"name":        "renamed-host",
	})
	require.Empty(t, missing)
	assert.Equal(t, map[string]any{"UHostId": "uhost-1", "Name": "renamed-host"}, args)

	updates = TaskSlotUpdatesFromUserText("ReinstallInstanceWorkflow", []string{"image_id"}, "Ubuntu-nvidia 22.04")
	assert.Nil(t, updates, "natural image names must be resolved by context/model + image tools, not treated as image IDs")

	updates = TaskSlotUpdatesFromUserText("ReinstallInstanceWorkflow", []string{"image_id"}, "img-ubuntu")
	assert.Equal(t, map[string]string{"image_id": "img-ubuntu"}, updates)
	args, missing = TaskArgsFromSlots("ReinstallInstanceWorkflow", map[string]string{
		"instance_id": "uhost-1",
		"image_pref":  "Ubuntu-nvidia 22.04",
	})
	require.Empty(t, missing)
	assert.Equal(t, map[string]any{"UHostId": "uhost-1", "ImageName": "Ubuntu-nvidia 22.04"}, args)
}
