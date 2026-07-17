package workflow

import (
	"fmt"
	"strings"
)

func EnableNetOptimizerDef() *Definition {
	return &Definition{
		Name:        "EnableNetOptimizerWorkflow",
		Description: "查询网络加速状态 -> 确认开启 -> 开启/同步网络加速 -> 回查状态",
		Steps: []Step{
			stepQueryNetOptimizerStatus(),
			stepConfirmEnableNetOptimizer(),
			stepSyncNetOptimizer(),
			stepRecheckNetOptimizerStatus(),
		},
		ResultData: func(wfCtx *Context) map[string]any {
			return map[string]any{
				"already_optimized": netOptimizerEnabled(wfCtx.Result("查询网络加速状态")),
				"before":            wfCtx.Result("查询网络加速状态"),
				"after":             wfCtx.Result("回查网络加速状态"),
			}
		},
	}
}

func stepQueryNetOptimizerStatus() Step {
	return Step{
		Name: "查询网络加速状态",
		Type: StepToolCall,
		Tool: "CheckCompShareNetOptimizer",
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			if err := normalizeNetOptimizerParams(wfCtx); err != nil {
				return nil, err
			}
			args := map[string]any{
				"Zone":     wfCtx.Params["Zone"],
				"Region":   wfCtx.Params["Region"],
				"az_group": wfCtx.Params["NetOptimizerAzGroup"],
			}
			addWorkflowIdentityArgs(args, wfCtx.Runtime)
			return args, nil
		},
	}
}

func stepConfirmEnableNetOptimizer() Step {
	return Step{
		Name: "确认开启网络加速",
		Type: StepConfirm,
		SkipIf: func(wfCtx *Context) (bool, error) {
			return netOptimizerEnabled(wfCtx.Result("查询网络加速状态")), nil
		},
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			status := wfCtx.Result("查询网络加速状态")
			return map[string]any{
				"Zone":      wfCtx.Params["Zone"],
				"Region":    wfCtx.Params["Region"],
				"optimized": netOptimizerEnabled(status),
				"status":    status,
				"warning":   "将为当前账号同步/开启网络加速配置；本轮 agent 暂不暴露关闭能力，确认后会调用开通同步接口。",
			}, nil
		},
	}
}

func stepSyncNetOptimizer() Step {
	return Step{
		Name: "开启网络加速",
		Type: StepToolCall,
		Tool: "SyncCompShareNetOptimizer",
		SkipIf: func(wfCtx *Context) (bool, error) {
			return netOptimizerEnabled(wfCtx.Result("查询网络加速状态")), nil
		},
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			args := map[string]any{
				"Zone":     wfCtx.Params["Zone"],
				"Region":   wfCtx.Params["Region"],
				"az_group": wfCtx.Params["NetOptimizerAzGroup"],
			}
			addWorkflowIdentityArgs(args, wfCtx.Runtime)
			return args, nil
		},
	}
}

func stepRecheckNetOptimizerStatus() Step {
	return Step{
		Name:     "回查网络加速状态",
		Type:     StepToolCall,
		Tool:     "CheckCompShareNetOptimizer",
		Optional: true,
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			args := map[string]any{
				"Zone":     wfCtx.Params["Zone"],
				"Region":   wfCtx.Params["Region"],
				"az_group": wfCtx.Params["NetOptimizerAzGroup"],
			}
			addWorkflowIdentityArgs(args, wfCtx.Runtime)
			return args, nil
		},
	}
}

// netOptimizerAzGroup reads the zone's internal region id (az_group) from the
// turn's zone catalog snapshot. Like workflowZonePlacement the snapshot is
// authoritative: the legacy map is read ONLY when no snapshot is attached (an
// unmigrated test). A snapshot that is present but unavailable, or that lacks the
// zone, is a hard failure — never a fall-through to a stale map. Removed with the
// map in S6.
func netOptimizerAzGroup(wfCtx *Context, zone string) (uint32, error) {
	cat := wfCtx.ZoneCatalog()
	if cat == nil {
		return guidedZoneRegionID(wfCtx.Params, zone), nil
	}
	if !cat.Available() {
		return 0, fmt.Errorf("可用区目录当前不可用，无法安全开启网络加速")
	}
	p, ok := cat.Placement(zone)
	if !ok {
		return 0, fmt.Errorf("可用区 %s 不在当前可用区目录中，无法安全开启网络加速", zone)
	}
	return p.AzGroup, nil
}

func normalizeNetOptimizerParams(wfCtx *Context) error {
	zone := strings.TrimSpace(paramStr(wfCtx.Params, "Zone", ""))
	if zone == "" {
		return NewMissingSlotError("开启网络加速需要指定可用区。", "zone")
	}
	region := strings.TrimSpace(paramStr(wfCtx.Params, "Region", ""))
	if region == "" {
		region = regionFromZone(zone)
	}
	if region == "" {
		return fmt.Errorf("无法从可用区推导地域，请同时指定 Region。")
	}
	azGroup, err := netOptimizerAzGroup(wfCtx, zone)
	if err != nil {
		return err
	}
	if azGroup == 0 {
		return fmt.Errorf("未获取到可用区 %s 的内部区域编号，无法安全开启网络加速。", zone)
	}
	wfCtx.Params["Zone"] = zone
	wfCtx.Params["Region"] = region
	wfCtx.Params["NetOptimizerAzGroup"] = azGroup
	return nil
}

func netOptimizerEnabled(result map[string]any) bool {
	if result == nil {
		return false
	}
	if optimized, ok := result["Optimized"].(bool); ok && optimized {
		return true
	}
	info, ok := result["Info"].([]any)
	if !ok || len(info) == 0 {
		return false
	}
	allOptimized := true
	for _, item := range info {
		row, ok := item.(map[string]any)
		if !ok {
			return false
		}
		if optimized, ok := row["Optimized"].(bool); !ok || !optimized {
			allOptimized = false
			break
		}
	}
	return allOptimized
}
