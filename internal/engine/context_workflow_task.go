package engine

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/compshare-agent/internal/entity"
	"github.com/compshare-agent/internal/workflow"
)

func workflowTaskSlotUpdatesFromUserText(workflowName string, missing []string, userMsg string) map[string]string {
	return workflow.TaskSlotUpdatesFromUserText(workflowName, missing, userMsg)
}

func sanitizeWorkflowTaskSlotUpdates(frame ContextFrame, updates map[string]string, userMsg string) map[string]string {
	proposed := strings.TrimSpace(updates["instance_id"])
	if proposed == "" {
		return updates
	}
	existing := strings.TrimSpace(frame.Slots["instance_id"])
	if existing != "" && strings.EqualFold(existing, proposed) {
		return updates
	}
	if userTextContainsInstanceID(userMsg, proposed) {
		return updates
	}
	cleaned := make(map[string]string, len(updates))
	for k, v := range updates {
		if strings.EqualFold(strings.TrimSpace(k), "instance_id") {
			continue
		}
		cleaned[k] = v
	}
	return cleaned
}

func trustedWorkflowFrameTarget(frame ContextFrame, slots map[string]string, userMsg string) string {
	target := strings.TrimSpace(slots["instance_id"])
	if target == "" {
		return ""
	}
	if contextFrameSlotSourceTrusted(frame, "instance_id") {
		if existing := strings.TrimSpace(frame.Slots["instance_id"]); existing != "" && strings.EqualFold(existing, target) {
			return target
		}
	}
	if userTextContainsInstanceID(userMsg, target) {
		return target
	}
	return ""
}

func userTextContainsInstanceID(userMsg, target string) bool {
	target = strings.TrimSpace(target)
	if target == "" {
		return false
	}
	snapshot := entity.RegistrySnapshot{Instances: map[string]entity.InstanceSnapshot{
		target: {UHostId: target},
	}}
	for _, token := range snapshot.InstanceIDTokensInText(userMsg) {
		if strings.EqualFold(strings.TrimSpace(token), target) {
			return true
		}
	}
	return false
}

func contextFrameSlotSourceTrusted(frame ContextFrame, slot string) bool {
	if frame.SlotSources == nil {
		return false
	}
	return strings.TrimSpace(frame.SlotSources[slot]) == SelectedInstanceSourceUser
}

func workflowArgsFromTaskSlots(workflowName string, slots map[string]string) (map[string]any, []string) {
	return workflow.TaskArgsFromSlots(workflowName, slots)
}

func (e *Engine) bindSelectedInstanceToWaitingWorkflowFrame(inst entity.InstanceSnapshot) bool {
	frame, ok := e.activeContextFrame(time.Now())
	if !ok || frame.Kind != ContextFrameKindWorkflowTask ||
		!workflowRequiresInstanceTarget(frame.Workflow) ||
		strings.TrimSpace(frame.Slots["instance_id"]) != "" ||
		!containsString(frame.MissingSlots, "instance_id") {
		return false
	}
	frame.Slots = mergeStringMaps(frame.Slots, map[string]string{"instance_id": inst.UHostId})
	frame.SlotSources = mergeStringMaps(frame.SlotSources, map[string]string{"instance_id": SelectedInstanceSourceUser})
	frame.ProducedAtUnix = time.Now().Unix()
	e.setContextFrame(frame)
	return true
}

func (e *Engine) recordWorkflowMissingSlotsFrame(workflowName string, args map[string]any, missing []string, message string) bool {
	if e == nil || !e.sessionStateHydrated || workflowName == "" || len(missing) == 0 {
		return false
	}
	slots := safeWorkflowContextSlots(args)
	slotSources := e.workflowContextSlotSources(workflowName, slots)
	if workflowRequiresInstanceTarget(workflowName) && strings.TrimSpace(slots["instance_id"]) == "" {
		if selected := strings.TrimSpace(e.sessionState.SelectedInstanceID); selected != "" &&
			selectedInstanceSourceTrustedForWorkflow(e.sessionState.SelectedInstanceSource) {
			if slots == nil {
				slots = map[string]string{}
			}
			slots["instance_id"] = selected
			if slotSources == nil {
				slotSources = map[string]string{}
			}
			slotSources["instance_id"] = SelectedInstanceSourceUser
		}
	}
	if frame, ok := e.activeContextFrame(time.Now()); ok && frame.Kind == ContextFrameKindWorkflowTask && frame.Workflow == workflowName {
		slots = mergeStringMaps(frame.Slots, slots)
		slotSources = mergeStringMaps(frame.SlotSources, slotSources)
	}
	frame := ContextFrame{
		Version:         1,
		Kind:            ContextFrameKindWorkflowTask,
		Status:          ContextFrameStatusFailedRecoverable,
		Intent:          string(e.lastPlannerIntentThisTurn),
		Workflow:        workflowName,
		OriginalUserMsg: strings.TrimSpace(e.lastUserMsg),
		Slots:           slots,
		SlotSources:     slotSources,
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

func (e *Engine) workflowContextSlotSources(workflowName string, slots map[string]string) map[string]string {
	if !workflowRequiresInstanceTarget(workflowName) || len(slots) == 0 {
		return nil
	}
	target := strings.TrimSpace(slots["instance_id"])
	if target == "" {
		return nil
	}
	if strings.TrimSpace(e.lastUserMsg) == "" {
		return nil
	}
	if e.workflowTargetIsTrusted(workflowName, target, false) {
		return map[string]string{"instance_id": SelectedInstanceSourceUser}
	}
	return nil
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
	put("description", args["Description"])
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
	for _, values := range []map[string]string{base, updates} {
		for k, v := range values {
			k = strings.TrimSpace(k)
			v = strings.TrimSpace(v)
			if k != "" && v != "" {
				out[k] = v
			}
		}
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
