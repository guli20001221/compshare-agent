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
	"github.com/compshare-agent/internal/workflow"
)

func (e *Engine) tryResumeWorkflowContextFrame(ctx context.Context, dispatch routerDispatchResult, userMsg string, onStep func(StepEvent)) (string, bool) {
	if !ContextContinuationEnabled() {
		return "", false
	}
	frame, ok := e.activeContextFrame(time.Now())
	if !ok || frame.Kind != ContextFrameKindWorkflowTask || strings.TrimSpace(frame.Workflow) == "" {
		return "", false
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
	updates := decision.SlotUpdates
	if len(updates) == 0 {
		updates = workflowTaskSlotUpdatesFromUserText(frame.Workflow, frame.MissingSlots, userMsg)
	}
	if len(updates) == 0 {
		if strings.TrimSpace(decision.Clarify) != "" {
			return e.deployReply(dispatch.result, dispatch.latency, strings.TrimSpace(decision.Clarify))
		}
		return "", false
	}
	return e.resumeWorkflowContextFrameWithSlotUpdates(ctx, dispatch, userMsg, frame, updates, onStep)
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
	return workflow.TaskSlotUpdatesFromUserText(workflowName, missing, userMsg)
}

func workflowArgsFromTaskSlots(workflowName string, slots map[string]string) (map[string]any, []string) {
	return workflow.TaskArgsFromSlots(workflowName, slots)
}

func (e *Engine) recordWorkflowMissingSlotsFrame(workflowName string, args map[string]any, missing []string, message string) bool {
	if e == nil || !ContextContinuationEnabled() || !e.sessionStateHydrated || workflowName == "" || len(missing) == 0 {
		return false
	}
	slots := safeWorkflowContextSlots(args)
	if workflowRequiresInstanceTarget(workflowName) && strings.TrimSpace(slots["instance_id"]) == "" {
		if selected := strings.TrimSpace(e.sessionState.SelectedInstanceID); selected != "" {
			if slots == nil {
				slots = map[string]string{}
			}
			slots["instance_id"] = selected
		}
	}
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
	put("cfs_id", firstNonNil(args["CfsId"], args["CFSId"]))
	put("size_gb", args["Size"])
	put("target_size_gb", firstNonNil(args["TargetSize"], args["DiskSpace"]))
	put("cpu", args["Cpu"])
	put("memory_gb", args["Memory"])
	put("gpu_count", args["Gpu"])
	put("gpu_type", args["GpuType"])
	put("zone", args["Zone"])
	put("image_id", args["CompShareImageId"])
	put("image_pref", args["ImageName"])
	put("image_source", args["ImageSource"])
	put("workload", args["Workload"])
	put("stop_time", firstNonNil(args["StopTime"], args["AfterMinutes"], args["ShutdownAt"]))
	put("name", args["Name"])
	put("charge_type", args["ChargeType"])
	if len(out) == 0 {
		return nil
	}
	return out
}

func workflowMissingSlotsClarification(workflowName string, missing []string) string {
	return workflow.TaskMissingSlotsClarification(workflowName, missing)
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
