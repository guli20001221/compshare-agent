package engine

import (
	"context"
	"errors"
	"testing"

	"github.com/compshare-agent/internal/tools"
	"github.com/compshare-agent/internal/workflow"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAuthorizedWriteFailureReadbackCoversEveryCompoundMutation(t *testing.T) {
	tests := []struct {
		name       string
		action     string
		step       string
		params     map[string]any
		sent       map[string]any
		readAction string
		readback   map[string]any
		writeErr   error
		contains   []string
	}{
		{
			name:   "ordinary start ran but archived GPU restore did not land",
			action: "StartInstanceWorkflow", step: "开机",
			params:     map[string]any{"UHostId": "uhost-1", "StartMode": "normal", "StartInitialGPU": float64(0), "StartInitialCPU": float64(2), "StartInitialMemory": float64(4096)},
			readAction: "DescribeCompShareInstance",
			readback:   instanceReadback("uhost-1", "Running", 0, "4090", 2, 4096, nil),
			contains:   []string{"已运行", "未观察到确认卡所示的原带卡规格恢复", "只完成了启动部分", "请勿重复提交"},
		},
		{
			name:   "pod reboot stopped after fallback",
			action: "RebootInstanceWorkflow", step: "重启",
			params:     map[string]any{"UHostId": "cpod-1", "RebootInitialStartTime": float64(100)},
			readAction: "DescribeCompShareInstance",
			readback:   instanceReadback("cpod-1", "Stopped", 1, "H20", 16, 245760, nil),
			contains:   []string{"已处于已关机", "停止阶段可能已经发生", "请勿重复提交"},
		},
		{
			name:   "instance resize target was committed despite error",
			action: "ResizeInstanceWorkflow", step: "变配",
			params:     map[string]any{"UHostId": "uhost-1", "ResizeInitialCPU": float64(16), "ResizeInitialGPU": float64(1), "ResizeInitialMemory": float64(65536)},
			sent:       map[string]any{"Cpu": float64(32), "Gpu": float64(2), "Memory": float64(131072)},
			readAction: "DescribeCompShareInstance",
			readback:   instanceReadback("uhost-1", "Stopped", 2, "4090", 32, 131072, nil),
			contains:   []string{"已是确认的目标规格", "4090 × 2 / 32核 / 128GB", "请勿重复提交"},
		},
		{
			name:   "instance resize partially committed",
			action: "ResizeInstanceWorkflow", step: "变配",
			params:     map[string]any{"UHostId": "uhost-1", "ResizeInitialCPU": float64(16), "ResizeInitialGPU": float64(1), "ResizeInitialMemory": float64(65536)},
			sent:       map[string]any{"Cpu": float64(32), "Gpu": float64(2), "Memory": float64(131072)},
			readAction: "DescribeCompShareInstance",
			readback:   instanceReadback("uhost-1", "Stopped", 2, "4090", 16, 65536, nil),
			contains:   []string{"不完全一致", "可能只完成了部分变更", "请勿重复提交"},
		},
		{
			name:   "disk resize target was committed despite error",
			action: "ResizeDiskWorkflow", step: "扩已有盘",
			params:     map[string]any{"UHostId": "uhost-1", "ResolvedDiskId": "disk-1", "CurrentDiskSize": float64(100), "Size": float64(200)},
			readAction: "DescribeCompShareInstance",
			readback: instanceReadback("uhost-1", "Stopped", 1, "H20", 16, 245760, []any{
				map[string]any{"DiskId": "disk-1", "Size": float64(200)},
			}),
			contains: []string{"disk-1 已达到 200GB", "请勿重复提交"},
		},
		{
			name:   "create CFS missing readback stays unknown",
			action: "CreateCFSWorkflow", step: "创建 CFS",
			params:     map[string]any{"Name": "shared-train", "Size": float64(100), "Zone": "cn-bj2-03", "Region": "cn-bj2", "CFSZoneId": float64(9001)},
			readAction: "DescribeCFS",
			readback:   map[string]any{"CFSSet": []any{}},
			contains:   []string{"不能证明创建没有发生", "结果不确定", "请勿重复创建"},
		},
		{
			name:   "create CFS was committed despite error",
			action: "CreateCFSWorkflow", step: "创建 CFS",
			params:     map[string]any{"Name": "shared-train", "Size": float64(100), "Zone": "cn-bj2-03", "Region": "cn-bj2", "CFSZoneId": float64(9001)},
			readAction: "DescribeCFS",
			readback: map[string]any{"CFSSet": []any{
				map[string]any{"CfsId": "cfs-new", "Name": "shared-train", "Size": float64(100), "ZoneId": float64(9001)},
			}},
			contains: []string{"已找到 CFS shared-train", "cfs-new", "请勿重复创建"},
		},
		{
			name:   "CFS resize partially committed",
			action: "ResizeCFSWorkflow", step: "扩容 CFS",
			params:     map[string]any{"CfsId": "cfs-1", "CurrentCFSSize": float64(100), "Size": float64(300)},
			readAction: "DescribeCFS",
			readback: map[string]any{"CFSSet": []any{
				map[string]any{"CfsId": "cfs-1", "Size": float64(200)},
			}},
			contains: []string{"当前为 200GB", "可能只完成了部分扩容", "请勿重复提交"},
		},
		{
			name:   "charge type switch committed despite error",
			action: "SwitchChargeTypeWorkflow", step: "切换计费方式",
			params:     map[string]any{"UHostId": "uhost-1", "DestChargeType": "Month"},
			readAction: "DescribeCompShareInstance",
			readback: map[string]any{"UHostSet": []any{map[string]any{
				"UHostId": "uhost-1", "ChargeType": "Month",
			}}},
			writeErr: tools.NewUpstreamAPIError(520, "balance not enough"),
			contains: []string{"账号余额不足", "主记录已显示为包月", "完整计费结果仍不确定", "可能只完成了一部分", "请先充值", "刷新平台账单和当前计费方式", "请勿直接重复旧卡"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			executor := &mockExecutorFn{fn: func(action string, args map[string]any) (map[string]any, error) {
				require.Equal(t, tt.readAction, action)
				if tt.action == "CreateCFSWorkflow" {
					require.Equal(t, uint32(9001), args["zone_id"])
				}
				return tt.readback, nil
			}}
			eng := NewWithDeps(&mockLLM{}, executor, nil)
			writeErr := tt.writeErr
			if writeErr == nil {
				writeErr = errors.New("synthetic write failure")
			}
			reply, ok := eng.authorizedWriteFailureReply(context.Background(), tt.action, tt.params, &workflow.Result{
				Err:     writeErr,
				Failure: &workflow.StepFailure{Step: tt.step, Args: tt.sent, ExecutionAuthorized: true},
			})
			require.True(t, ok)
			for _, want := range tt.contains {
				assert.Contains(t, reply, want)
			}
			if tt.action == "SwitchChargeTypeWorkflow" {
				assert.NotContains(t, reply, "已切换为",
					"the instance row cannot prove that the later related-storage billing steps completed")
			}
			assert.Equal(t, []string{tt.readAction}, executor.calls)
		})
	}
}

func TestAuthorizedWriteFailureReadbackNeverRunsBeforeTheWriteWasAuthorized(t *testing.T) {
	executor := &mockExecutorFn{fn: func(string, map[string]any) (map[string]any, error) {
		t.Fatal("an unapproved failure must not trigger a reconciliation read")
		return nil, nil
	}}
	eng := NewWithDeps(&mockLLM{}, executor, nil)

	reply, ok := eng.authorizedWriteFailureReply(context.Background(), "CreateCFSWorkflow", map[string]any{
		"Name": "shared-train", "Zone": "cn-bj2-03",
	}, &workflow.Result{Failure: &workflow.StepFailure{Step: "创建 CFS", ExecutionAuthorized: false}})

	assert.False(t, ok)
	assert.Empty(t, reply)
	assert.Empty(t, executor.calls)
}

func TestReinstallFailureReadbackOnlyClaimsAnObservedImageChange(t *testing.T) {
	tests := []struct {
		name       string
		initialID  string
		observedID string
		contains   string
		notContain string
	}{
		{
			name: "target differs from the pre-confirm image", initialID: "img-old", observedID: "img-new",
			contains: "已切换到目标镜像",
		},
		{
			name: "same-image reinstall cannot be inferred from readback", initialID: "img-new", observedID: "img-new",
			contains: "不能确认重装已完整生效", notContain: "已切换到目标镜像",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			executor := &mockExecutorFn{fn: func(action string, _ map[string]any) (map[string]any, error) {
				require.Equal(t, "DescribeCompShareInstance", action)
				result := instanceReadback("uhost-1", "Stopped", 1, "H20", 16, 245760, nil)
				row := result["UHostSet"].([]any)[0].(map[string]any)
				row["CompShareImageId"] = tt.observedID
				row["ImageName"] = "Ubuntu 22.04"
				return result, nil
			}}
			eng := NewWithDeps(&mockLLM{}, executor, nil)
			reply, ok := eng.authorizedWriteFailureReply(context.Background(), "ReinstallInstanceWorkflow", map[string]any{
				"UHostId": "uhost-1",
			}, &workflow.Result{
				Err: errors.New("synthetic reinstall failure"),
				Failure: &workflow.StepFailure{
					Step: "重装系统", ExecutionAuthorized: true,
					Draft: map[string]any{
						"InitialImageId": tt.initialID, "TargetImageId": "img-new", "TargetImageName": "Ubuntu 22.04",
					},
				},
			})
			require.True(t, ok)
			assert.Contains(t, reply, tt.contains)
			if tt.notContain != "" {
				assert.NotContains(t, reply, tt.notContain)
			}
		})
	}
}

func TestRebootFailureUsesThePreConfirmStartTimeToConfirmACompletedReboot(t *testing.T) {
	describeCalls := 0
	executor := &mockExecutorFn{fn: func(action string, _ map[string]any) (map[string]any, error) {
		switch action {
		case "DescribeCompShareInstance":
			describeCalls++
			result := instanceReadback("uhost-1", "Running", 1, "H20", 16, 245760, nil)
			row := result["UHostSet"].([]any)[0].(map[string]any)
			if describeCalls == 1 {
				row["StartTime"] = float64(100)
			} else {
				row["StartTime"] = float64(200)
			}
			return result, nil
		case "DescribeCompShareSupportZone":
			return map[string]any{"ZoneInfo": []any{}}, nil
		case "RebootCompShareInstance":
			return nil, errors.New("synthetic reboot transport failure")
		default:
			return map[string]any{"RetCode": 0}, nil
		}
	}}
	eng := NewWithDeps(&mockLLM{}, executor, func(string, map[string]any) bool { return true })

	reply := eng.executeResolvedWorkflow(context.Background(), mustConfirmable("RebootInstanceWorkflow", map[string]any{
		"UHostId": "uhost-1",
	}, zoneRefData(nil)), noopStep)

	assert.Contains(t, reply, "已重新运行")
	assert.Contains(t, reply, "启动时间已经更新")
	assert.Contains(t, reply, "请勿重复提交")
	assert.Equal(t, []string{
		"DescribeCompShareInstance", "DescribeCompShareSupportZone", "RebootCompShareInstance", "DescribeCompShareInstance",
	}, executor.calls)
}

func instanceReadback(id, state string, gpu int, gpuType string, cpu, memory int, disks []any) map[string]any {
	row := map[string]any{
		"UHostId": id, "State": state, "GPU": gpu, "GpuType": gpuType,
		"CPU": cpu, "Memory": memory, "Zone": "cn-wlcb-01", "Region": "cn-wlcb",
	}
	if disks != nil {
		row["DiskSet"] = disks
	}
	return map[string]any{"UHostSet": []any{row}}
}
