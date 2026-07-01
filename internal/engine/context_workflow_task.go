package engine

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	openai "github.com/sashabaranov/go-openai"

	"github.com/compshare-agent/internal/intent"
	"github.com/compshare-agent/internal/observability"
)

func (e *Engine) tryResumeWorkflowContextFrame(ctx context.Context, dispatch routerDispatchResult, userMsg string, onStep func(StepEvent)) (string, bool) {
	if !ContextContinuationEnabled() {
		return "", false
	}
	frame, ok := e.activeContextFrame(time.Now())
	if !ok || frame.Kind != ContextFrameKindWorkflowTask || strings.TrimSpace(frame.Workflow) == "" {
		return "", false
	}
	if updates := workflowTaskSlotUpdatesFromUserText(frame.Workflow, frame.MissingSlots, userMsg); len(updates) > 0 {
		return e.resumeWorkflowContextFrameWithSlotUpdates(ctx, dispatch, userMsg, frame, updates, onStep)
	}
	decision, err := e.resolveContextDecision(ctx, userMsg, dispatch.result.Plan.Intent, frame)
	if err != nil || decision == nil {
		return "", false
	}
	switch decision.Decision {
	case ContextDecisionNewTask, ContextDecisionClearContext:
		e.clearContextFrame()
		return "", false
	case ContextDecisionClarify:
		if decision.Clarify != "" {
			return e.deployReply(dispatch.result, dispatch.latency, decision.Clarify)
		}
		return "", false
	case ContextDecisionContinueTask:
	default:
		return "", false
	}
	if len(decision.SlotUpdates) == 0 {
		if strings.TrimSpace(decision.Clarify) != "" {
			return e.deployReply(dispatch.result, dispatch.latency, strings.TrimSpace(decision.Clarify))
		}
		return "", false
	}
	return e.resumeWorkflowContextFrameWithSlotUpdates(ctx, dispatch, userMsg, frame, decision.SlotUpdates, onStep)
}

func (e *Engine) resumeWorkflowContextFrameWithSlotUpdates(ctx context.Context, dispatch routerDispatchResult, _ string, frame ContextFrame, updates map[string]string, onStep func(StepEvent)) (string, bool) {
	slots := mergeStringMaps(frame.Slots, updates)
	args, missing := workflowArgsFromTaskSlots(frame.Workflow, slots)
	if len(missing) > 0 {
		next := frame
		next.Slots = slots
		next.MissingSlots = missing
		next.FailureReason = workflowMissingSlotsClarification(frame.Workflow, missing)
		next.ProducedAtUnix = time.Now().Unix()
		e.setContextFrame(next)
		return e.deployReply(dispatch.result, dispatch.latency, next.FailureReason)
	}

	e.clearContextFrame()
	action := frame.Workflow
	e.emitPlannerTrace(dispatch.result, intent.RouteStatusDispatchedAgent, dispatch.latency)
	args = e.safeExecutor.FilterArgs(action, args)
	onStep(StepEvent{
		Type:   StepToolCall,
		Action: action,
		Source: observability.ToolSourceMainReAct,
		Args:   e.safeExecutor.RedactArgs(action, args),
	})
	raw := e.executeWorkflow(ctx, action, args, onStep)
	reply := workflowDirectReply(action, raw)
	e.messages = append(e.messages, openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, Content: reply})
	return reply, true
}

func workflowTaskSlotUpdatesFromUserText(workflowName string, missing []string, userMsg string) map[string]string {
	missingSet := map[string]bool{}
	for _, slot := range missing {
		if normalized := normalizeContextSlotKey(slot); normalized != "" {
			missingSet[normalized] = true
		}
	}
	if len(missingSet) == 0 {
		return nil
	}
	updates := map[string]string{}
	putRawNumber := func(slot string) {
		if !missingSet[slot] {
			return
		}
		if _, ok := parseContextSlotNumber(userMsg); ok {
			updates[slot] = strings.TrimSpace(userMsg)
		}
	}
	switch workflowName {
	case "CreateDiskWorkflow":
		putRawNumber("size_gb")
	case "ResizeDiskWorkflow":
		putRawNumber("target_size_gb")
	case "ResizeInstanceWorkflow":
		for key, value := range resizeWorkflowArgsFromUserText(userMsg) {
			switch key {
			case "Cpu":
				if missingSet["cpu"] {
					updates["cpu"] = fmt.Sprint(value)
				}
			case "Memory":
				if missingSet["memory_gb"] {
					updates["memory_gb"] = fmt.Sprint(value)
				}
			case "Gpu":
				if missingSet["gpu_count"] {
					updates["gpu_count"] = fmt.Sprint(value)
				}
			}
		}
	}
	if len(updates) == 0 {
		return nil
	}
	return updates
}

func workflowArgsFromTaskSlots(workflowName string, slots map[string]string) (map[string]any, []string) {
	normalized := normalizeContextSlotUpdates(slots)
	args := map[string]any{}
	var missing []string
	if workflowRequiresInstanceTarget(workflowName) {
		if id := normalized["instance_id"]; id != "" {
			args["UHostId"] = id
		} else {
			missing = append(missing, "instance_id")
		}
	}
	if diskID := normalized["disk_id"]; diskID != "" {
		args["DiskId"] = diskID
		args["UDiskId"] = diskID
	}
	switch workflowName {
	case "CreateDiskWorkflow":
		if size, ok := parseContextSlotNumber(normalized["size_gb"]); ok {
			args["Size"] = size
		} else {
			missing = append(missing, "size_gb")
		}
	case "ResizeDiskWorkflow":
		if size, ok := parseContextSlotNumber(normalized["target_size_gb"]); ok {
			args["Size"] = size
		} else {
			missing = append(missing, "target_size_gb")
		}
	case "ResizeInstanceWorkflow":
		hasSpec := false
		if cpu, ok := parseContextSlotNumber(normalized["cpu"]); ok {
			args["Cpu"] = cpu
			hasSpec = true
		}
		if mem, ok := parseContextSlotNumber(normalized["memory_gb"]); ok {
			args["Memory"] = mem
			hasSpec = true
		}
		if gpu, ok := parseContextSlotNumber(normalized["gpu_count"]); ok {
			args["Gpu"] = gpu
			hasSpec = true
		}
		if !hasSpec {
			missing = append(missing, "cpu", "memory_gb", "gpu_count")
		}
	}
	if len(missing) > 0 {
		return map[string]any{}, uniqueStrings(missing)
	}
	return args, nil
}

func (e *Engine) recordWorkflowMissingSlotsFrame(workflowName string, args map[string]any, missing []string, message string) bool {
	if e == nil || !e.sessionStateHydrated || workflowName == "" || len(missing) == 0 {
		return false
	}
	slots := safeWorkflowContextSlots(args)
	if frame, ok := e.activeContextFrame(time.Now()); ok && frame.Kind == ContextFrameKindWorkflowTask && frame.Workflow == workflowName {
		slots = mergeStringMaps(frame.Slots, slots)
	}
	frame := ContextFrame{
		Version:         1,
		Kind:            ContextFrameKindWorkflowTask,
		Status:          ContextFrameStatusFailedRecoverable,
		Intent:          string(e.lastPlannerIntentThisTurn),
		Workflow:        workflowName,
		OriginalUserMsg: strings.TrimSpace(e.lastUserMsg),
		Slots:           slots,
		MissingSlots:    uniqueStrings(missing),
		Stage:           "missing_slots",
		FailureReason:   strings.TrimSpace(message),
		CreatedTurn:     e.userTurn,
		ProducedAtUnix:  time.Now().Unix(),
		TTLSeconds:      ContextFrameTTLSeconds,
	}
	e.setContextFrame(frame)
	return true
}

func safeWorkflowContextSlots(args map[string]any) map[string]string {
	out := map[string]string{}
	put := func(slot string, value any) {
		if s := contextSlotString(value); s != "" {
			out[slot] = s
		}
	}
	put("instance_id", args["UHostId"])
	put("disk_id", firstNonNil(args["DiskId"], args["UDiskId"]))
	put("size_gb", args["Size"])
	put("target_size_gb", firstNonNil(args["TargetSize"], args["DiskSpace"]))
	put("cpu", args["Cpu"])
	put("memory_gb", args["Memory"])
	put("gpu_count", args["Gpu"])
	put("gpu_type", args["GpuType"])
	put("zone", args["Zone"])
	put("image_pref", args["ImageName"])
	put("image_source", args["ImageSource"])
	put("workload", args["Workload"])
	put("stop_time", args["StopTime"])
	put("name", args["Name"])
	if len(out) == 0 {
		return nil
	}
	return out
}

func workflowMissingSlotsClarification(workflowName string, missing []string) string {
	seen := map[string]bool{}
	for _, m := range missing {
		seen[m] = true
	}
	switch workflowName {
	case "CreateDiskWorkflow":
		if seen["size_gb"] {
			return "需要先确认数据盘大小。请告诉我要加多大的数据盘，例如 200GB。"
		}
	case "ResizeDiskWorkflow":
		if seen["target_size_gb"] {
			return "需要先确认扩容后的目标容量。请告诉我要扩到多少 GB，例如 200GB。"
		}
	case "ResizeInstanceWorkflow":
		if seen["cpu"] || seen["memory_gb"] || seen["gpu_count"] {
			return "需要先确认目标配置。请告诉我要改成多少 CPU、内存或 GPU，例如 4C8G。"
		}
	}
	return "还缺少必要参数，无法安全继续。请补充后我再为你确认。"
}

func mergeStringMaps(base, updates map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range normalizeContextSlotUpdates(base) {
		out[k] = v
	}
	for k, v := range normalizeContextSlotUpdates(updates) {
		out[k] = v
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func parseContextSlotNumber(value string) (float64, bool) {
	v := strings.TrimSpace(strings.ToLower(value))
	if v == "" {
		return 0, false
	}
	v = strings.ReplaceAll(v, " ", "")
	v = strings.TrimSuffix(v, "gb")
	v = strings.TrimSuffix(v, "g")
	v = strings.TrimSuffix(v, "c")
	n, err := strconv.ParseFloat(v, 64)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

func contextSlotString(value any) string {
	switch v := value.(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(v)
	case int:
		return strconv.Itoa(v)
	case int64:
		return strconv.FormatInt(v, 10)
	case float64:
		return formatContextSlotFloat(v)
	case float32:
		return formatContextSlotFloat(float64(v))
	default:
		return strings.TrimSpace(fmt.Sprint(v))
	}
}

func formatContextSlotFloat(v float64) string {
	if v == float64(int64(v)) {
		return strconv.FormatInt(int64(v), 10)
	}
	return strconv.FormatFloat(v, 'f', -1, 64)
}

func firstNonNil(values ...any) any {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return nil
}
