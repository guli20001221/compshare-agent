package engine

import (
	"context"
	"fmt"
	"strings"

	"github.com/compshare-agent/internal/entity"
	"github.com/compshare-agent/internal/readprojection"
	"github.com/compshare-agent/internal/workflow"
)

// cpuOnlyStartFailureReply reconciles the one partial-commit shape of
// StartCompShareInstance: upstream may persist the requested CPU-only spec
// before it starts the instance. A failed start therefore needs one readback
// before we can truthfully say what state the request left behind.
func (e *Engine) cpuOnlyStartFailureReply(ctx context.Context, params map[string]any, result *workflow.Result) (string, bool) {
	if result == nil || result.Success || result.Failure == nil ||
		result.Failure.Step != "开机" || !result.Failure.ExecutionAuthorized {
		return "", false
	}
	cpu, memory, ok := cpuOnlyResourcesFromStartArgs(result.Failure.Args)
	if !ok {
		return "", false
	}
	id := strings.TrimSpace(fmt.Sprint(params["UHostId"]))
	if id == "" {
		return cpuOnlyStartUnknownReply(), true
	}

	snap, _, ok := e.freshInstance(ctx, id)
	if !ok {
		return cpuOnlyStartUnknownReply(), true
	}
	state := readprojection.ResourceStateLabel(snap.State)
	want := fmt.Sprintf("%d核/%dGB 的 CPU-only 规格", cpu, memory/1024)
	if snap.GPU == 0 && snap.CPU == cpu && snap.Memory == memory {
		switch strings.ToLower(strings.TrimSpace(snap.State)) {
		case "running":
			return fmt.Sprintf("无卡开机接口返回失败，但实时回读显示实例 %s 当前为 %s，且已运行。请以实时状态为准，请勿重复提交。", id, want), true
		case "starting", "initializing", "install", "installing":
			return fmt.Sprintf("无卡开机接口返回失败，但实时回读显示实例 %s 当前为 %s，状态为%s。请勿重复提交，稍后再查看状态。", id, want, state), true
		case "stopped":
			return fmt.Sprintf("无卡开机接口返回失败；实时回读显示实例 %s 当前为 %s，截至本次回读仍处于%s，开机结果尚未确认。请勿重复提交，请稍后刷新状态。", id, want, state), true
		default:
			return fmt.Sprintf("无卡开机接口返回失败；实时回读显示实例 %s 当前为 %s，状态为%s。请以实时状态为准，请勿重复提交。", id, want, state), true
		}
	}

	return fmt.Sprintf("无卡开机接口返回失败；实时回读显示实例 %s 当前状态为%s，实际规格为%s，不能确认无卡改配已完整生效。请勿重复提交，请先按当前状态处理。",
		id, state, observedInstanceSpec(snap)), true
}

func cpuOnlyResourcesFromStartArgs(args map[string]any) (cpu, memory int, ok bool) {
	spec := strings.ToUpper(strings.TrimSpace(fmt.Sprint(args["WithoutGpuSpec"])))
	switch spec {
	case "A":
		return 2, 4096, true
	case "B":
		return 8, 16384, true
	default:
		return 0, 0, false
	}
}

func describedInstanceByID(raw map[string]any, id string) map[string]any {
	rows, _ := raw["UHostSet"].([]any)
	for _, item := range rows {
		row, _ := item.(map[string]any)
		if strings.EqualFold(strings.TrimSpace(fmt.Sprint(row["UHostId"])), id) {
			return row
		}
	}
	return nil
}

func observedInstanceSpec(snap entity.InstanceSnapshot) string {
	gpu := fmt.Sprintf("%d GPU", snap.GPU)
	if snap.GPU > 0 && strings.TrimSpace(snap.GpuType) != "" {
		gpu = fmt.Sprintf("%s × %d", snap.GpuType, snap.GPU)
	}
	return fmt.Sprintf("%s / %d核 / %dGB", gpu, snap.CPU, snap.Memory/1024)
}

func cpuOnlyStartUnknownReply() string {
	return "无卡开机接口返回失败。由于平台会先改配再开机，而随后未能读取实例当前状态，本次结果不确定。请勿重复提交，请先查询实例状态和规格。"
}
