package workflow

import (
	"fmt"
	"math"
	"time"
)

// nowFunc is the time source used by resolveShutdownTime. Tests override it
// to inject a fixed clock.
var nowFunc = time.Now

// shanghaiLoc is preloaded so we never pay the cost of LoadLocation at
// call-time and never have to handle the (impossible-in-practice) error.
var shanghaiLoc *time.Location

func init() {
	var err error
	shanghaiLoc, err = time.LoadLocation("Asia/Shanghai")
	if err != nil {
		// Fallback to a fixed +08:00 offset if tzdata is missing.
		shanghaiLoc = time.FixedZone("CST", 8*3600)
	}
}

// resolveShutdownTime converts user-supplied shutdown parameters into a unix
// timestamp and a human-readable display string.
//
// Exactly one of AfterMinutes or ShutdownAt must be provided.
//
//   - AfterMinutes: positive integer >= 5.
//   - ShutdownAt:   RFC3339 ("2006-01-02T15:04:05Z07:00") or plain
//     "2006-01-02 15:04" interpreted in Asia/Shanghai.
//
// The resolved time must be at least 5 minutes in the future.
func resolveShutdownTime(params map[string]any) (unix int64, display string, err error) {
	_, hasAfter := params["AfterMinutes"]
	_, hasAt := params["ShutdownAt"]

	if !hasAfter && !hasAt {
		return 0, "", fmt.Errorf("请指定关机时间（AfterMinutes 或 ShutdownAt）")
	}
	if hasAfter && hasAt {
		return 0, "", fmt.Errorf("AfterMinutes 和 ShutdownAt 不能同时指定，请只传其中一个")
	}

	now := nowFunc()

	if hasAfter {
		raw := paramNum(params, "AfterMinutes", 0)
		// Must be a positive integer (no fractional part).
		if raw != math.Trunc(raw) || raw < 1 {
			return 0, "", fmt.Errorf("AfterMinutes 必须是正整数")
		}
		minutes := int64(raw)
		if minutes < 5 {
			return 0, "", fmt.Errorf("AfterMinutes 至少为 5 分钟")
		}
		target := now.Add(time.Duration(minutes) * time.Minute)
		return target.Unix(), formatShutdownDisplay(target, now), nil
	}

	// ShutdownAt path.
	atStr, _ := params["ShutdownAt"].(string)
	var target time.Time

	// Try RFC3339 first.
	if t, e := time.Parse(time.RFC3339, atStr); e == nil {
		target = t
	} else {
		// Fall back to plain datetime in Asia/Shanghai.
		t2, e2 := time.ParseInLocation("2006-01-02 15:04", atStr, shanghaiLoc)
		if e2 != nil {
			return 0, "", fmt.Errorf("ShutdownAt 格式无法识别，请使用 RFC3339（如 2006-01-02T15:04:05+08:00）或 \"2006-01-02 15:04\"")
		}
		target = t2
	}

	// Final validation: must be at least 5 minutes from now.
	if target.Before(now.Add(5 * time.Minute)) {
		return 0, "", fmt.Errorf("关机时间必须至少在当前时间的 5 分钟之后")
	}

	return target.Unix(), formatShutdownDisplay(target, now), nil
}

// formatShutdownDisplay renders a human-readable shutdown time, e.g.
// "2026-04-16 23:00（北京时间，约 2 小时后）".
func formatShutdownDisplay(target, now time.Time) string {
	beijing := target.In(shanghaiLoc)
	ts := beijing.Format("2006-01-02 15:04")

	diff := target.Sub(now).Round(time.Minute)
	relative := formatRelativeDuration(diff)

	return fmt.Sprintf("%s（北京时间，%s）", ts, relative)
}

// formatRelativeDuration converts a duration into a friendly Chinese string
// like "约 2 小时后" or "约 30 分钟后".
func formatRelativeDuration(d time.Duration) string {
	minutes := int(d.Minutes())
	if minutes < 60 {
		return fmt.Sprintf("约 %d 分钟后", minutes)
	}
	hours := minutes / 60
	remainMin := minutes % 60
	if remainMin == 0 {
		return fmt.Sprintf("约 %d 小时后", hours)
	}
	return fmt.Sprintf("约 %d 小时 %d 分钟后", hours, remainMin)
}

// ---------------------------------------------------------------------------
// SetStopScheduler workflow
// ---------------------------------------------------------------------------

// SetStopSchedulerDef returns the 3-step workflow definition for setting a
// scheduled stop on a CompShare GPU instance: query state, confirm, then set.
func SetStopSchedulerDef() *Definition {
	return &Definition{
		Name:        "SetStopSchedulerWorkflow",
		Description: "查询实例 → 确认设置 → 设置定时关机",
		Steps: []Step{
			stepQueryForScheduler(),
			stepConfirmScheduler(),
			stepSetStopScheduler(),
		},
	}
}

func stepQueryForScheduler() Step {
	return Step{
		Name: "查询实例",
		Type: StepToolCall,
		Tool: "DescribeCompShareInstance",
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			return map[string]any{
				"UHostIds": []any{wfCtx.Params["UHostId"]},
			}, nil
		},
		CheckResult: func(_ *Context, result map[string]any) (bool, string) {
			state := extractInstanceState(result)
			switch state {
			case "":
				return false, "未找到该实例。"
			case "Running":
				chargeType := extractField(result, "ChargeType")
				if chargeType == "Spot" {
					return false, "抢占式实例不支持定时关机。"
				}
				return true, ""
			default:
				return false, fmt.Sprintf("实例当前未运行（状态：%s），无需设置定时关机。", state)
			}
		},
	}
}

func stepConfirmScheduler() Step {
	return Step{
		Name: "确认设置",
		Type: StepConfirm,
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			_, display, err := resolveShutdownTime(wfCtx.Params)
			if err != nil {
				return nil, err
			}
			summary := extractInstanceSummary(wfCtx.Result("查询实例"))
			summary["shutdownTime"] = display
			return summary, nil
		},
	}
}

func stepSetStopScheduler() Step {
	return Step{
		Name: "设置定时关机",
		Type: StepToolCall,
		Tool: "UpdateCompShareStopScheduler",
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			unix, _, err := resolveShutdownTime(wfCtx.Params)
			if err != nil {
				return nil, err
			}
			queried := wfCtx.Result("查询实例")
			return map[string]any{
				"Region":            extractInstanceRegion(queried, defaultRegion),
				"Zone":              extractInstanceZone(queried, defaultZone),
				"UHostId":           wfCtx.Params["UHostId"],
				"SchedulerStopTime": unix,
			}, nil
		},
	}
}

// ---------------------------------------------------------------------------
// CancelStopScheduler workflow
// ---------------------------------------------------------------------------

// CancelStopSchedulerDef returns the 3-step workflow definition for cancelling
// a scheduled stop on a CompShare GPU instance: query state, confirm, then
// delete the scheduler task.
func CancelStopSchedulerDef() *Definition {
	return &Definition{
		Name:        "CancelStopSchedulerWorkflow",
		Description: "查询实例 → 确认取消 → 取消定时关机",
		Steps: []Step{
			stepQueryForCancelScheduler(),
			stepConfirmCancelScheduler(),
			stepDeleteStopScheduler(),
		},
	}
}

func stepQueryForCancelScheduler() Step {
	return Step{
		Name: "查询实例",
		Type: StepToolCall,
		Tool: "DescribeCompShareInstance",
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			return map[string]any{
				"UHostIds": []any{wfCtx.Params["UHostId"]},
			}, nil
		},
		CheckResult: func(_ *Context, result map[string]any) (bool, string) {
			state := extractInstanceState(result)
			if state == "" {
				return false, "未找到该实例。"
			}
			// Any state is allowed — stopped instances may have residual
			// scheduler tasks that should be cleaned up.
			return true, ""
		},
	}
}

func stepConfirmCancelScheduler() Step {
	return Step{
		Name: "确认取消",
		Type: StepConfirm,
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			summary := extractInstanceSummary(wfCtx.Result("查询实例"))
			summary["warning"] = "将尝试取消该实例的定时关机任务。"
			return summary, nil
		},
	}
}

func stepDeleteStopScheduler() Step {
	return Step{
		Name: "取消定时关机",
		Type: StepToolCall,
		Tool: "DeleteCompShareStopScheduler",
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			return map[string]any{
				"UHostId": wfCtx.Params["UHostId"],
			}, nil
		},
	}
}

// extractField returns a string field from the first UHostSet entry, or "".
func extractField(result map[string]any, key string) string {
	if result == nil {
		return ""
	}
	hostSet, ok := result["UHostSet"].([]any)
	if !ok || len(hostSet) == 0 {
		return ""
	}
	first, ok := hostSet[0].(map[string]any)
	if !ok {
		return ""
	}
	if v, ok := first[key].(string); ok {
		return v
	}
	return ""
}
