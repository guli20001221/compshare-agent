package engine

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/compshare-agent/internal/intent"
)

type ContextContinuationDecision struct {
	Decision     string `json:"decision"`
	GPUPref      string `json:"gpu_pref"`
	ZonePref     string `json:"zone_pref"`
	ImagePref    string `json:"image_pref"`
	ImageSource  string `json:"image_source"`
	WorkloadPref string `json:"workload_pref"`
	InstanceRef  string `json:"instance_ref"`
	Clarify      string `json:"clarify"`
	Reason       string `json:"reason"`
}

const (
	ContextContinuationContinue = "continue_task"
	ContextContinuationNew      = "new_question"
	ContextContinuationClear    = "clear_context"
	ContextContinuationClarify  = "clarify"
)

func (e *Engine) resolveContextContinuation(ctx context.Context, userMsg string, route intent.Intent, frame ContextFrame) (*ContextContinuationDecision, error) {
	decision, err := e.resolveContextDecision(ctx, userMsg, route, frame)
	if err != nil || decision == nil {
		return nil, err
	}
	return contextDecisionToContinuation(*decision), nil
}

func summarizeContextFrame(frame ContextFrame) string {
	var lines []string
	add := func(k, v string) {
		if strings.TrimSpace(v) != "" {
			lines = append(lines, k+": "+strings.TrimSpace(v))
		}
	}
	add("kind", frame.Kind)
	add("status", frame.Status)
	add("workflow", frame.Workflow)
	add("original_user_msg", frame.OriginalUserMsg)
	if len(frame.MissingSlots) > 0 {
		add("missing_slots", strings.Join(frame.MissingSlots, ","))
	}
	if len(frame.Slots) > 0 {
		keys := make([]string, 0, len(frame.Slots))
		for k := range frame.Slots {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var pairs []string
		for _, k := range keys {
			if v := strings.TrimSpace(frame.Slots[k]); v != "" {
				pairs = append(pairs, k+"="+v)
			}
		}
		if len(pairs) > 0 {
			add("slots", strings.Join(pairs, ","))
		}
	}
	add("gpu", frame.GPU)
	add("image_pref", frame.ImagePref)
	add("image_source", frame.ImageSource)
	add("workload", frame.Workload)
	add("zone", frame.Zone)
	add("zone_label", frame.ZoneLabel)
	add("stage", frame.Stage)
	add("failure_reason", frame.FailureReason)
	if len(frame.AlternativeZones) > 0 {
		var zs []string
		for _, z := range frame.AlternativeZones {
			label := strings.TrimSpace(z.Label)
			if label == "" {
				label = strings.TrimSpace(z.Zone)
			}
			if label != "" {
				zs = append(zs, label)
			}
		}
		if len(zs) > 0 {
			add("alternative_zones", strings.Join(zs, "、"))
		}
	}
	if len(lines) == 0 {
		return "none"
	}
	return strings.Join(lines, "\n")
}

func summarizeInstanceContext(state SessionState) string {
	var lines []string
	if state.SelectedInstanceID != "" {
		if state.SelectedInstanceName != "" {
			lines = append(lines, "selected_instance: "+state.SelectedInstanceName+" ("+state.SelectedInstanceID+")")
		} else {
			lines = append(lines, "selected_instance: "+state.SelectedInstanceID)
		}
	}
	if len(state.PendingSelectionItems) > 0 {
		var items []string
		for _, item := range state.PendingSelectionItems {
			if item.Index <= 0 || item.ID == "" {
				continue
			}
			label := item.Name
			if label == "" {
				label = item.ID
			}
			items = append(items, fmt.Sprintf("%d:%s(%s)", item.Index, label, item.ID))
			if len(items) >= 5 {
				break
			}
		}
		if len(items) > 0 {
			lines = append(lines, "recent_instance_list: "+strings.Join(items, ", "))
		}
	}
	return strings.Join(lines, "\n")
}
