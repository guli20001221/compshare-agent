package engine

import (
	"context"
	"fmt"
	"strings"

	"github.com/compshare-agent/internal/entity"
	"github.com/compshare-agent/internal/readprojection"
	"github.com/compshare-agent/internal/tools"
	"github.com/compshare-agent/internal/workflow"
)

// authorizedWriteFailureReply reconciles only failures from an attempted,
// user-authorized mutation. A failed API response is not proof that a compound
// upstream operation made no change, so each operation performs one fresh read
// and reports only the state that read establishes. It never retries or rolls
// back the write.
func (e *Engine) authorizedWriteFailureReply(ctx context.Context, action string, params map[string]any, result *workflow.Result) (string, bool) {
	if result == nil || result.Success || result.Err == nil || result.Failure == nil || !result.Failure.ExecutionAuthorized {
		return "", false
	}
	wantStep := map[string]string{
		"StartInstanceWorkflow":     "开机",
		"RebootInstanceWorkflow":    "重启",
		"ResizeInstanceWorkflow":    "变配",
		"ResizeDiskWorkflow":        "扩已有盘",
		"CreateCFSWorkflow":         "创建 CFS",
		"ResizeCFSWorkflow":         "扩容 CFS",
		"ReinstallInstanceWorkflow": "重装系统",
		"SwitchChargeTypeWorkflow":  "切换计费方式",
	}[action]
	if wantStep == "" || result.Failure.Step != wantStep {
		return "", false
	}

	switch action {
	case "StartInstanceWorkflow":
		if reply, ok := e.cpuOnlyStartFailureReply(ctx, params, result); ok {
			return reply, true
		}
		return e.ordinaryStartFailureReply(ctx, params, result), true
	case "RebootInstanceWorkflow":
		return e.rebootFailureReply(ctx, params), true
	case "ResizeInstanceWorkflow":
		return e.resizeInstanceFailureReply(ctx, params, result.Failure.Args), true
	case "ResizeDiskWorkflow":
		return e.resizeDiskFailureReply(ctx, params), true
	case "CreateCFSWorkflow":
		return e.createCFSFailureReply(ctx, params), true
	case "ResizeCFSWorkflow":
		return e.resizeCFSFailureReply(ctx, params), true
	case "ReinstallInstanceWorkflow":
		return e.reinstallFailureReply(ctx, params, result)
	case "SwitchChargeTypeWorkflow":
		return e.switchChargeTypeFailureReply(ctx, params, result), true
	default:
		return "", false
	}
}

func (e *Engine) switchChargeTypeFailureReply(ctx context.Context, params map[string]any, result *workflow.Result) string {
	id := strings.TrimSpace(fmt.Sprint(params["UHostId"]))
	target := strings.TrimSpace(fmt.Sprint(params["DestChargeType"]))
	failure := "计费方式切换接口返回失败"
	afterFailure := "请勿重复提交。"
	if result != nil {
		if apiErr, found := tools.UpstreamAPIErrorFrom(result.Err); found && apiErr.Code == 520 {
			failure = "账号余额不足"
			afterFailure = "请先充值并刷新平台账单和当前计费方式，确认状态后再决定是否重新发起；请勿直接重复旧卡。"
		}
	}
	snap, _, ok := e.freshInstance(ctx, id)
	if !ok {
		return fmt.Sprintf("%s；随后未能读取实例 %s 的当前计费方式，本次结果不确定。%s", failure, id, afterFailure)
	}
	observed := strings.TrimSpace(snap.ChargeType)
	if observed != "" && strings.EqualFold(observed, target) {
		return fmt.Sprintf("%s；实时回读仅能确认实例 %s 的主记录已显示为%s，关联存储和完整计费结果仍不确定，可能只完成了一部分。请以平台账单为准。%s", failure, id, workflow.ChargeTypeLabel(observed), afterFailure)
	}
	if observed == "" {
		return fmt.Sprintf("%s；实时回读未返回实例 %s 的计费方式，无法确认本次结果。%s", failure, id, afterFailure)
	}
	return fmt.Sprintf("%s；实时回读显示实例 %s 当前仍为%s，尚未确认切换为%s。该接口包含多步结算和计费修改，本次结果可能不完整。%s",
		failure, id, workflow.ChargeTypeLabel(observed), workflow.ChargeTypeLabel(target), afterFailure)
}

func (e *Engine) freshInstance(ctx context.Context, id string) (entity.InstanceSnapshot, map[string]any, bool) {
	id = strings.TrimSpace(id)
	if id == "" {
		return entity.InstanceSnapshot{}, nil, false
	}
	raw, err := e.executeRawTool(ctx, "DescribeCompShareInstance", map[string]any{
		"UHostIds": []any{id},
	}, tools.OriginWorkflowInternal)
	if err != nil {
		return entity.InstanceSnapshot{}, nil, false
	}
	row := describedInstanceByID(raw, id)
	if row == nil {
		return entity.InstanceSnapshot{}, nil, false
	}
	return entity.InstanceFromMap(row), row, true
}

func (e *Engine) ordinaryStartFailureReply(ctx context.Context, params map[string]any, result *workflow.Result) string {
	id := strings.TrimSpace(fmt.Sprint(params["UHostId"]))
	snap, _, ok := e.freshInstance(ctx, id)
	if !ok {
		return fmt.Sprintf("实例 %s 的开机请求返回失败，随后未能读取当前状态，本次结果不确定。请勿重复提交，请先查询实例状态和规格。", id)
	}
	state := readprojection.ResourceStateLabel(snap.State)
	spec := observedInstanceSpec(snap)
	initialGPU, initialGPUOK := firstNumberAny(params, "StartInitialGPU")
	_, initialCPUOK := firstNumberAny(params, "StartInitialCPU")
	_, initialMemoryOK := firstNumberAny(params, "StartInitialMemory")
	startedFromCPUOnly := initialGPUOK && initialCPUOK && initialMemoryOK && initialGPU == 0
	switch strings.ToLower(strings.TrimSpace(snap.State)) {
	case "running":
		if startedFromCPUOnly && snap.GPU == 0 {
			return fmt.Sprintf("开机接口返回失败；实时回读显示实例 %s 已运行，但当前仍是%s，未观察到确认卡所示的原带卡规格恢复。本次只完成了启动部分，请勿重复提交。", id, spec)
		}
		return fmt.Sprintf("开机接口返回失败，但实时回读显示实例 %s 已运行，当前规格为%s。请以实时状态为准，请勿重复提交。", id, spec)
	case "starting", "initializing", "install", "installing":
		return fmt.Sprintf("开机接口返回失败，但实时回读显示实例 %s 正处于%s，当前规格为%s。请求可能仍在处理中，请勿重复提交。", id, state, spec)
	case "stopped":
		if startedFromCPUOnly && snap.GPU > 0 {
			return fmt.Sprintf("开机接口返回失败；实时回读显示实例 %s 仍处于%s，但规格已从 CPU-only 变为%s。GPU 规格恢复已发生，启动尚未完成，请勿重复提交。", id, state, spec)
		}
		if apiErr, found := tools.UpstreamAPIErrorFrom(result.Err); found && (apiErr.Code == 8357 || apiErr.Code == 226604) {
			return fmt.Sprintf("实例 %s 的原带卡规格当前库存不足，本次没有启动完成，也不会自动改成无卡规格。实时状态为%s，当前规格为%s；请勿重复提交，可稍后再试。", id, state, spec)
		}
		return fmt.Sprintf("开机接口返回失败；实时回读显示实例 %s 截至本次回读仍处于%s，当前规格为%s，未观察到启动完成。请勿重复提交，请稍后再查看状态。", id, state, spec)
	default:
		return fmt.Sprintf("开机接口返回失败；实时回读显示实例 %s 当前状态为%s，规格为%s。请以实时状态为准，请勿重复提交。", id, state, spec)
	}
}

func (e *Engine) rebootFailureReply(ctx context.Context, params map[string]any) string {
	id := strings.TrimSpace(fmt.Sprint(params["UHostId"]))
	snap, _, ok := e.freshInstance(ctx, id)
	if !ok {
		return fmt.Sprintf("实例 %s 的重启请求返回失败，随后未能读取当前状态，本次结果不确定。请勿重复提交。", id)
	}
	state := readprojection.ResourceStateLabel(snap.State)
	switch strings.ToLower(strings.TrimSpace(snap.State)) {
	case "rebooting", "starting", "initializing", "install", "installing":
		return fmt.Sprintf("重启接口返回失败，但实时回读显示实例 %s 正处于%s，请求可能仍在处理中。请勿重复提交。", id, state)
	case "stopped":
		return fmt.Sprintf("重启接口返回失败；实时回读显示实例 %s 已处于%s。平台重启的停止阶段可能已经发生，但重新启动尚未完成，请勿重复提交。", id, state)
	case "running":
		initialStart, hasInitial := firstNumberAny(params, "RebootInitialStartTime")
		if hasInitial && initialStart > 0 && snap.StartTime > int64(initialStart) {
			return fmt.Sprintf("重启接口返回失败，但实时回读显示实例 %s 已重新运行，启动时间已经更新。请以实时状态为准，请勿重复提交。", id)
		}
		return fmt.Sprintf("重启接口返回失败；实时回读显示实例 %s 当前仍在运行，但无法确认本次重启是否发生。请勿重复提交。", id)
	default:
		return fmt.Sprintf("重启接口返回失败；实时回读显示实例 %s 当前状态为%s。请以实时状态为准，请勿重复提交。", id, state)
	}
}

func (e *Engine) resizeInstanceFailureReply(ctx context.Context, params, sent map[string]any) string {
	id := strings.TrimSpace(fmt.Sprint(params["UHostId"]))
	snap, _, ok := e.freshInstance(ctx, id)
	if !ok {
		return fmt.Sprintf("实例 %s 的变配请求返回失败，随后未能读取实际规格，本次结果不确定。请勿重复提交。", id)
	}
	wantCPU, cpuOK := firstNumberAny(sent, "Cpu", "CPU")
	wantGPU, gpuOK := firstNumberAny(sent, "Gpu", "GPU")
	wantMemory, memoryOK := firstNumberAny(sent, "Memory")
	if cpuOK && gpuOK && memoryOK && float64(snap.CPU) == wantCPU && float64(snap.GPU) == wantGPU && float64(snap.Memory) == wantMemory {
		return fmt.Sprintf("变配接口返回失败，但实时回读显示实例 %s 已是确认的目标规格：%s。请以实时规格为准，请勿重复提交。", id, observedInstanceSpec(snap))
	}
	initialCPU, initialCPUOK := firstNumberAny(params, "ResizeInitialCPU")
	initialGPU, initialGPUOK := firstNumberAny(params, "ResizeInitialGPU")
	initialMemory, initialMemoryOK := firstNumberAny(params, "ResizeInitialMemory")
	if initialCPUOK && initialGPUOK && initialMemoryOK && float64(snap.CPU) == initialCPU && float64(snap.GPU) == initialGPU && float64(snap.Memory) == initialMemory {
		return fmt.Sprintf("变配接口返回失败；实时回读显示实例 %s 仍是变配前规格：%s，未观察到目标变配生效。请勿重复提交。", id, observedInstanceSpec(snap))
	}
	return fmt.Sprintf("变配接口返回失败；实时回读显示实例 %s 当前规格为%s，与确认的目标规格不完全一致，可能只完成了部分变更。请勿重复提交。", id, observedInstanceSpec(snap))
}

func (e *Engine) resizeDiskFailureReply(ctx context.Context, params map[string]any) string {
	id := strings.TrimSpace(fmt.Sprint(params["UHostId"]))
	_, row, ok := e.freshInstance(ctx, id)
	if !ok {
		return fmt.Sprintf("实例 %s 的磁盘扩容请求返回失败，随后未能读取实际磁盘容量，本次结果不确定。请勿重复提交。", id)
	}
	diskID := strings.TrimSpace(fmt.Sprint(params["ResolvedDiskId"]))
	target, targetOK := firstNumberAny(params, "Size")
	initial, initialOK := firstNumberAny(params, "CurrentDiskSize")
	disk := instanceDiskByID(row, diskID)
	if disk == nil {
		return fmt.Sprintf("磁盘扩容请求返回失败，实时回读未找到磁盘 %s，无法确认结果。请勿重复提交。", diskID)
	}
	actual, actualOK := firstNumberAny(disk, "Size", "DiskSpace")
	if !actualOK || !targetOK {
		return fmt.Sprintf("磁盘扩容请求返回失败，随后未能确认磁盘 %s 的实际容量。本次结果不确定，请勿重复提交。", diskID)
	}
	if actual >= target {
		return fmt.Sprintf("磁盘扩容接口返回失败，但实时回读显示磁盘 %s 已达到 %.0fGB。请以实时容量为准，请勿重复提交。", diskID, actual)
	}
	if initialOK && actual <= initial {
		return fmt.Sprintf("磁盘扩容接口返回失败；实时回读显示磁盘 %s 仍为 %.0fGB，未观察到目标 %.0fGB 生效。请勿重复提交。", diskID, actual, target)
	}
	return fmt.Sprintf("磁盘扩容接口返回失败；实时回读显示磁盘 %s 当前为 %.0fGB，尚未达到目标 %.0fGB，可能只完成了部分扩容。请勿重复提交。", diskID, actual, target)
}

func instanceDiskByID(row map[string]any, id string) map[string]any {
	set, _ := row["DiskSet"].([]any)
	for _, item := range set {
		disk, _ := item.(map[string]any)
		for _, key := range []string{"DiskId", "UDiskId", "Id", "DiskShortId"} {
			if strings.EqualFold(strings.TrimSpace(fmt.Sprint(disk[key])), id) {
				return disk
			}
		}
	}
	return nil
}

func (e *Engine) createCFSFailureReply(ctx context.Context, params map[string]any) string {
	args := map[string]any{}
	if zoneID, ok := firstNumberAny(params, "CFSZoneId"); ok && zoneID > 0 {
		args["zone_id"] = uint32(zoneID)
	}
	for _, key := range []string{"Zone", "Region"} {
		if value := strings.TrimSpace(fmt.Sprint(params[key])); value != "" {
			args[key] = value
		}
	}
	raw, err := e.executeRawTool(ctx, "DescribeCFS", args, tools.OriginWorkflowInternal)
	if err != nil {
		return "CFS 创建请求返回失败，随后未能读取该可用区的 CFS，本次结果不确定。请勿重复创建。"
	}
	rows := cfsRows(raw)
	if len(rows) == 0 {
		return "CFS 创建请求返回失败；实时回读暂未找到该可用区的 CFS，但这不能证明创建没有发生。本次结果不确定，请勿重复创建。"
	}
	wantName := strings.TrimSpace(fmt.Sprint(params["Name"]))
	wantSize, _ := firstNumberAny(params, "Size")
	wantZoneID, hasZoneID := firstNumberAny(params, "CFSZoneId")
	for _, row := range rows {
		if hasZoneID {
			actualZoneID, ok := firstNumberAny(row, "ZoneId", "ZoneID", "zone_id")
			if !ok || actualZoneID != wantZoneID {
				continue
			}
		}
		name := strings.TrimSpace(fmt.Sprint(row["Name"]))
		actual, _ := firstNumberAny(row, "Size")
		if name == wantName {
			id := cfsID(row)
			if actual >= wantSize && wantSize > 0 {
				return fmt.Sprintf("CFS 创建接口返回失败，但实时回读已找到 CFS %s（%s，%.0fGB）。请以实时结果为准，请勿重复创建。", name, id, actual)
			}
			return fmt.Sprintf("CFS 创建接口返回失败；实时回读已找到同名 CFS %s（%s，当前 %.0fGB），但与确认的目标容量不完全一致。结果可能只完成了一部分，请勿重复创建。", name, id, actual)
		}
	}
	return "CFS 创建请求返回失败；实时回读发现该可用区已有 CFS，但无法确认它是否来自本次请求。本次结果不确定，请勿重复创建。"
}

func (e *Engine) resizeCFSFailureReply(ctx context.Context, params map[string]any) string {
	id := strings.TrimSpace(fmt.Sprint(params["CfsId"]))
	raw, err := e.executeRawTool(ctx, "DescribeCFS", map[string]any{"CfsId": id}, tools.OriginWorkflowInternal)
	if err != nil {
		return fmt.Sprintf("CFS %s 的扩容请求返回失败，随后未能读取实际容量，本次结果不确定。请勿重复提交。", id)
	}
	row := cfsRowByID(raw, id)
	if row == nil {
		return fmt.Sprintf("CFS %s 的扩容请求返回失败，实时回读也未找到该 CFS，无法确认结果。请勿重复提交。", id)
	}
	actual, actualOK := firstNumberAny(row, "Size")
	target, targetOK := firstNumberAny(params, "Size")
	initial, initialOK := firstNumberAny(params, "CurrentCFSSize")
	if !actualOK || !targetOK {
		return fmt.Sprintf("CFS %s 的扩容请求返回失败，随后未能确认实际容量。本次结果不确定，请勿重复提交。", id)
	}
	if actual >= target {
		return fmt.Sprintf("CFS 扩容接口返回失败，但实时回读显示 CFS %s 已达到 %.0fGB。请以实时容量为准，请勿重复提交。", id, actual)
	}
	if initialOK && actual <= initial {
		return fmt.Sprintf("CFS 扩容接口返回失败；实时回读显示 CFS %s 仍为 %.0fGB，未观察到目标 %.0fGB 生效。请勿重复提交。", id, actual, target)
	}
	return fmt.Sprintf("CFS 扩容接口返回失败；实时回读显示 CFS %s 当前为 %.0fGB，尚未达到目标 %.0fGB，可能只完成了部分扩容。请勿重复提交。", id, actual, target)
}

func cfsRows(raw map[string]any) []map[string]any {
	set, _ := raw["CFSSet"].([]any)
	out := make([]map[string]any, 0, len(set))
	for _, item := range set {
		if row, ok := item.(map[string]any); ok {
			out = append(out, row)
		}
	}
	if len(out) == 0 && cfsID(raw) != "" {
		out = append(out, raw)
	}
	return out
}

func cfsRowByID(raw map[string]any, id string) map[string]any {
	for _, row := range cfsRows(raw) {
		if strings.EqualFold(cfsID(row), id) {
			return row
		}
	}
	return nil
}

func cfsID(row map[string]any) string {
	for _, key := range []string{"CfsId", "CFSId"} {
		if value := strings.TrimSpace(fmt.Sprint(row[key])); value != "" && value != "<nil>" {
			return value
		}
	}
	return ""
}
