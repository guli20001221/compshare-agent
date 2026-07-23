package workflow

import (
	"fmt"
	"math"
	"strings"
	"time"
)

// nowFunc is the time source used by resolveShutdownTime. Tests override it
// to inject a fixed clock.
var nowFunc = time.Now

const (
	shutdownDraftStepName = "形成定时关机草稿"
	shutdownDraftParamKey = "__shutdown_schedule_draft"
)

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

// resolveShutdownTime converts the typed Schedule value into the one timestamp
// shown on the confirmation card and sent upstream. Relative calendar words are
// never converted by the model: today/tomorrow arithmetic lives here.
func resolveShutdownTime(params map[string]any) (unix int64, display string, err error) {
	raw, exists := params["Schedule"]
	if !exists {
		return 0, "", NewMissingSlotError("请指定关机时间。", "schedule")
	}
	schedule, ok := raw.(map[string]any)
	if !ok {
		return 0, "", fmt.Errorf("Schedule 必须是结构化时间")
	}
	mode, _ := schedule["mode"].(string)
	now := nowFunc()
	loc, err := shutdownScheduleLocation(schedule)
	if err != nil {
		return 0, "", err
	}
	var target time.Time
	switch mode {
	case "after_minutes":
		if scheduleFieldPresent(schedule, "local_time", "at") {
			return 0, "", fmt.Errorf("mode=after_minutes 不允许 local_time 或 at")
		}
		minutesValue, ok := numericValue(schedule["minutes"])
		if !ok || minutesValue != math.Trunc(minutesValue) {
			return 0, "", fmt.Errorf("Schedule.minutes 必须是正整数")
		}
		minutes := int64(minutesValue)
		if minutes < 5 {
			return 0, "", fmt.Errorf("Schedule.minutes 至少为 5 分钟")
		}
		target = now.Add(time.Duration(minutes) * time.Minute)
	case "today", "tomorrow":
		if scheduleFieldPresent(schedule, "minutes", "at") {
			return 0, "", fmt.Errorf("mode=%s 不允许 minutes 或 at", mode)
		}
		clock, _ := schedule["local_time"].(string)
		parsedClock, parseErr := time.ParseInLocation("15:04", strings.TrimSpace(clock), loc)
		if parseErr != nil {
			return 0, "", fmt.Errorf("Schedule.local_time 必须使用 HH:MM 格式")
		}
		localNow := now.In(loc)
		year, month, day := localNow.Date()
		target = time.Date(year, month, day, parsedClock.Hour(), parsedClock.Minute(), 0, 0, loc)
		if mode == "tomorrow" {
			target = target.AddDate(0, 0, 1)
		}
	case "absolute":
		if scheduleFieldPresent(schedule, "minutes", "local_time") {
			return 0, "", fmt.Errorf("mode=absolute 不允许 minutes 或 local_time")
		}
		at, _ := schedule["at"].(string)
		if parsed, parseErr := time.Parse(time.RFC3339, strings.TrimSpace(at)); parseErr == nil {
			target = parsed
		} else if parsed, parseErr := time.ParseInLocation("2006-01-02 15:04", strings.TrimSpace(at), loc); parseErr == nil {
			target = parsed
		} else {
			return 0, "", fmt.Errorf("Schedule.at 必须使用 RFC3339 或 YYYY-MM-DD HH:MM 格式")
		}
	default:
		return 0, "", fmt.Errorf("Schedule.mode 必须是 after_minutes、today、tomorrow 或 absolute")
	}
	if target.Before(now.Add(5 * time.Minute)) {
		return 0, "", fmt.Errorf("关机时间必须至少在当前时间的 5 分钟之后")
	}
	return target.Unix(), formatShutdownDisplay(target, now), nil
}

func scheduleFieldPresent(schedule map[string]any, names ...string) bool {
	for _, name := range names {
		if _, exists := schedule[name]; exists {
			return true
		}
	}
	return false
}

func shutdownScheduleLocation(schedule map[string]any) (*time.Location, error) {
	timezone, _ := schedule["timezone"].(string)
	switch strings.TrimSpace(timezone) {
	case "", "Asia/Shanghai":
		return shanghaiLoc, nil
	case "UTC":
		return time.UTC, nil
	default:
		return nil, fmt.Errorf("Schedule.timezone 仅支持 Asia/Shanghai 或 UTC")
	}
}

func numericValue(value any) (float64, bool) {
	switch typed := value.(type) {
	case int:
		return float64(typed), true
	case int64:
		return float64(typed), true
	case float64:
		return typed, true
	default:
		return 0, false
	}
}

// ValidateShutdownSchedule is the catalog-level structural gate. The workflow
// still resolves it against the live server clock immediately before showing
// the confirmation card.
func ValidateShutdownSchedule(value any) error {
	params := map[string]any{"Schedule": value}
	_, _, err := resolveShutdownTime(params)
	return err
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
		Name: "SetStopSchedulerWorkflow",
		Steps: []Step{
			stepQueryForScheduler(),
			stepResolveSchedulerDraft(),
			stepConfirmScheduler(),
			stepSetStopScheduler(),
		},
	}
}

func stepResolveSchedulerDraft() Step {
	return Step{
		Name: shutdownDraftStepName,
		Type: StepResolve,
		Resolve: func(wfCtx *Context) (map[string]any, error) {
			unix, display, err := resolveShutdownTime(wfCtx.Params)
			if err != nil {
				return nil, err
			}
			return map[string]any{
				"unix":    unix,
				"display": display,
			}, nil
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
		CheckResult: func(_ *Context, result map[string]any) CheckOutcome {
			state := extractInstanceState(result)
			switch state {
			case "":
				return CheckFailed("未找到该实例。")
			case "Running":
				if extractFirstBool(result, "IsSpot") {
					return CheckFailed("抢占式实例不支持定时关机。")
				}
				return CheckPassed()
			default:
				return CheckFailed(fmt.Sprintf("实例当前未运行（状态：%s），无需设置定时关机。", state))
			}
		},
	}
}

func stepConfirmScheduler() Step {
	return Step{
		Name: "确认设置",
		Type: StepConfirm,
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			draft, err := shutdownDraftFromResult(wfCtx.Result(shutdownDraftStepName))
			if err != nil {
				return nil, err
			}
			if _, _, err := extractRequiredInstanceLocation(wfCtx.Result("查询实例"), nil); err != nil {
				return nil, err
			}
			summary := extractInstanceSummary(wfCtx.Result("查询实例"))
			summary["shutdownTime"] = draft["display"]
			return summary, nil
		},
		PromoteOnConfirm: func(wfCtx *Context) error {
			draft, err := shutdownDraftFromResult(wfCtx.Result(shutdownDraftStepName))
			if err != nil {
				return err
			}
			wfCtx.Params[shutdownDraftParamKey] = deepCopyValue(draft)
			return nil
		},
	}
}

func stepSetStopScheduler() Step {
	return Step{
		Name: "设置定时关机",
		Type: StepToolCall,
		Tool: "UpdateCompShareStopScheduler",
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			draft, err := shutdownDraftFromValue(wfCtx.Params[shutdownDraftParamKey])
			if err != nil {
				return nil, err
			}
			queried := wfCtx.Result("查询实例")
			region, zone, err := extractRequiredInstanceLocation(queried, nil)
			if err != nil {
				return nil, err
			}
			return map[string]any{
				"Region":            region,
				"Zone":              zone,
				"UHostId":           wfCtx.Params["UHostId"],
				"SchedulerStopTime": draft["unix"],
			}, nil
		},
	}
}

func shutdownDraftFromResult(raw map[string]any) (map[string]any, error) {
	return shutdownDraftFromValue(raw)
}

func shutdownDraftFromValue(raw any) (map[string]any, error) {
	draft, ok := raw.(map[string]any)
	if !ok || draft == nil {
		return nil, fmt.Errorf("缺少已确认的定时关机草稿")
	}
	unix, ok := draft["unix"].(int64)
	if !ok || unix <= 0 {
		return nil, fmt.Errorf("定时关机草稿中的时间无效")
	}
	display, ok := draft["display"].(string)
	if !ok || strings.TrimSpace(display) == "" {
		return nil, fmt.Errorf("定时关机草稿中的展示时间无效")
	}
	return map[string]any{"unix": unix, "display": display}, nil
}

// ---------------------------------------------------------------------------
// CancelStopScheduler workflow
// ---------------------------------------------------------------------------

// CancelStopSchedulerDef returns the 3-step workflow definition for cancelling
// a scheduled stop on a CompShare GPU instance: query state, confirm, then
// delete the scheduler task.
func CancelStopSchedulerDef() *Definition {
	return &Definition{
		Name: "CancelStopSchedulerWorkflow",
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
		CheckResult: func(_ *Context, result map[string]any) CheckOutcome {
			state := extractInstanceState(result)
			if state == "" {
				return CheckFailed("未找到该实例。")
			}
			// Any state is allowed — stopped instances may have residual
			// scheduler tasks that should be cleaned up.
			return CheckPassed()
		},
	}
}

func stepConfirmCancelScheduler() Step {
	return Step{
		Name: "确认取消",
		Type: StepConfirm,
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			if _, _, err := extractRequiredInstanceLocation(wfCtx.Result("查询实例"), nil); err != nil {
				return nil, err
			}
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
			queried := wfCtx.Result("查询实例")
			region, _, err := extractRequiredInstanceLocation(queried, nil)
			if err != nil {
				return nil, err
			}
			return map[string]any{
				"Region":  region,
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
