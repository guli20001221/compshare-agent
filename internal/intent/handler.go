package intent

import (
	"fmt"
	"sort"
	"strings"

	"github.com/compshare-agent/internal/entity"
	"github.com/compshare-agent/internal/security"
)

const FriendlyToolFailureReply = "\u67e5\u8be2\u6682\u65f6\u5931\u8d25\uff0c\u8bf7\u7a0d\u540e\u518d\u8bd5\u3002"

type HandlerStatus string

const (
	HandlerStatusHandled            HandlerStatus = "handled"
	HandlerStatusFallbackBeforeTool HandlerStatus = "fallback_before_tool"
	HandlerStatusFailureAfterTool   HandlerStatus = "failure_after_tool"
)

type FallbackReason string

const (
	FallbackNone             FallbackReason = ""
	FallbackUnresolvedTarget FallbackReason = "unresolved_target"
	FallbackActionNotAllowed FallbackReason = "action_not_allowed"
)

type CutoverStatus string

const (
	CutoverStatusNone                     CutoverStatus = ""
	CutoverStatusDispatched               CutoverStatus = "dispatched"
	CutoverStatusFallbackInvalid          CutoverStatus = "fallback_invalid"
	CutoverStatusFallbackLowConfidence    CutoverStatus = "fallback_low_confidence"
	CutoverStatusFallbackHardBlockHint    CutoverStatus = "fallback_hard_block_hint"
	CutoverStatusFallbackIneligible       CutoverStatus = "fallback_ineligible"
	CutoverStatusFallbackUnresolvedTarget CutoverStatus = "fallback_unresolved_target"
	CutoverStatusFallbackTimeWindow       CutoverStatus = "fallback_time_window"
	CutoverStatusFailureAfterTool         CutoverStatus = "failure_after_tool"
)

type HandlerResult struct {
	Status         HandlerStatus
	Reply          string
	FallbackReason FallbackReason
	CutoverStatus  CutoverStatus
}

func HandledResult(reply string) HandlerResult {
	return HandlerResult{
		Status:        HandlerStatusHandled,
		Reply:         reply,
		CutoverStatus: CutoverStatusDispatched,
	}
}

func FallbackBeforeTool(reason FallbackReason) HandlerResult {
	return HandlerResult{
		Status:         HandlerStatusFallbackBeforeTool,
		FallbackReason: reason,
		CutoverStatus:  cutoverStatusForFallback(reason),
	}
}

func FailureAfterTool(label string, _ error) HandlerResult {
	reply := FriendlyToolFailureReply
	label = strings.TrimSpace(label)
	if label != "" {
		reply = label + ": " + reply
	}
	return HandlerResult{
		Status:        HandlerStatusFailureAfterTool,
		Reply:         reply,
		CutoverStatus: CutoverStatusFailureAfterTool,
	}
}

func cutoverStatusForFallback(reason FallbackReason) CutoverStatus {
	switch reason {
	case FallbackUnresolvedTarget:
		return CutoverStatusFallbackUnresolvedTarget
	case FallbackActionNotAllowed:
		return CutoverStatusFallbackIneligible
	default:
		return CutoverStatusFallbackInvalid
	}
}

var handlerActionWhitelist = map[Intent]map[string]struct{}{
	IntentResourceInfo: {
		"DescribeCompShareInstance": {},
	},
	IntentMonitorQuery: {
		"GetCompShareInstanceMonitor": {},
	},
}

func IsAllowedHandlerAction(intent Intent, action string) bool {
	allowed, ok := handlerActionWhitelist[intent]
	if !ok {
		return false
	}
	_, ok = allowed[action]
	return ok
}

func RequireAllowedHandlerAction(intent Intent, action string) HandlerResult {
	if IsAllowedHandlerAction(intent, action) {
		return HandledResult("")
	}
	return FallbackBeforeTool(FallbackActionNotAllowed)
}

func RenderResourceSummary(instances []entity.InstanceSnapshot) string {
	copied := append([]entity.InstanceSnapshot(nil), instances...)
	sort.Slice(copied, func(i, j int) bool {
		return copied[i].UHostId < copied[j].UHostId
	})
	if len(copied) == 0 {
		return "No instances found."
	}
	lines := make([]string, 0, len(copied))
	for _, inst := range copied {
		parts := []string{
			"UHostId=" + safeValue(inst.UHostId),
			"Name=" + safeValue(inst.Name),
			"State=" + safeValue(inst.State),
			"GpuType=" + safeValue(inst.GpuType),
			fmt.Sprintf("GPU=%d", inst.GPU),
			fmt.Sprintf("CPU=%d", inst.CPU),
			fmt.Sprintf("Memory=%d", inst.Memory),
		}
		if inst.ImageType != "" {
			parts = append(parts, "ImageType="+safeValue(inst.ImageType))
		}
		if inst.StartTime != 0 {
			parts = append(parts, fmt.Sprintf("StartTime=%d", inst.StartTime))
		}
		if inst.ExpireTime != 0 {
			parts = append(parts, fmt.Sprintf("ExpireTime=%d", inst.ExpireTime))
		}
		lines = append(lines, strings.Join(parts, ", "))
	}
	return strings.Join(lines, "\n")
}

func RenderMonitorSummary(metrics []Metric, payload map[string]any) string {
	redacted, _ := security.RedactForLLM(payload).(map[string]any)
	flat := map[string]string{}
	flattenScalars("", redacted, flat)
	if len(flat) == 0 {
		return "No monitor values returned."
	}

	keys := make([]string, 0, len(flat))
	for key := range flat {
		if len(metrics) == 0 || matchesRequestedMetric(key, metrics) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	if len(keys) == 0 {
		return "No requested monitor values returned."
	}

	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, key+"="+flat[key])
	}
	return strings.Join(parts, "; ")
}

func flattenScalars(prefix string, v any, out map[string]string) {
	switch typed := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			next := key
			if prefix != "" {
				next = prefix + "." + key
			}
			flattenScalars(next, typed[key], out)
		}
	case []any:
		for i, item := range typed {
			next := fmt.Sprintf("%s[%d]", prefix, i)
			flattenScalars(next, item, out)
		}
	default:
		if prefix != "" {
			out[prefix] = safeValue(typed)
		}
	}
}

func matchesRequestedMetric(key string, metrics []Metric) bool {
	key = strings.ToLower(key)
	for _, metric := range metrics {
		if strings.Contains(key, string(metric)) {
			return true
		}
	}
	return false
}

func safeValue(v any) string {
	return fmt.Sprint(security.RedactForLLM(v))
}

