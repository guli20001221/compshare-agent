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
	FallbackMissingTarget    FallbackReason = "missing_target"
	FallbackUnresolvedTarget FallbackReason = "unresolved_target"
	FallbackAmbiguousTarget  FallbackReason = "ambiguous_target"
	FallbackTimeWindow       FallbackReason = "time_window"
	FallbackValidation       FallbackReason = "validation"
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

func FailureAfterTool(label string) HandlerResult {
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
	case FallbackMissingTarget, FallbackUnresolvedTarget, FallbackAmbiguousTarget:
		return CutoverStatusFallbackUnresolvedTarget
	case FallbackTimeWindow:
		return CutoverStatusFallbackTimeWindow
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

func RequireAllowedHandlerAction(intent Intent, action string) *HandlerResult {
	if IsAllowedHandlerAction(intent, action) {
		return nil
	}
	result := FallbackBeforeTool(FallbackActionNotAllowed)
	return &result
}

const (
	resourceLabelInstanceID = "\u5b9e\u4f8bID"
	resourceLabelName       = "\u540d\u79f0"
	resourceLabelState      = "\u72b6\u6001"
	resourceLabelGPUType    = "GPU\u578b\u53f7"
	resourceLabelGPU        = "GPU\u6570\u91cf"
	resourceLabelCPU        = "CPU"
	resourceLabelMemory     = "\u5185\u5b58"
	resourceLabelImageType  = "\u955c\u50cf\u7c7b\u578b"
	resourceLabelStartTime  = "\u542f\u52a8\u65f6\u95f4"
	resourceLabelExpireTime = "\u5230\u671f\u65f6\u95f4"

	noInstancesReply              = "\u672a\u627e\u5230\u5b9e\u4f8b\u3002"
	noMonitorValuesReply          = "\u672a\u8fd4\u56de\u76d1\u63a7\u6570\u636e\u3002"
	noRequestedMonitorValuesReply = "\u672a\u8fd4\u56de\u8bf7\u6c42\u7684\u76d1\u63a7\u6307\u6807\u3002"
)

func RenderResourceSummary(instances []entity.InstanceSnapshot) string {
	copied := append([]entity.InstanceSnapshot(nil), instances...)
	sort.Slice(copied, func(i, j int) bool {
		return copied[i].UHostId < copied[j].UHostId
	})
	if len(copied) == 0 {
		return noInstancesReply
	}
	lines := make([]string, 0, len(copied))
	for _, inst := range copied {
		parts := []string{
			resourceLabelInstanceID + "=" + safeValue(inst.UHostId),
			resourceLabelName + "=" + safeValue(inst.Name),
			resourceLabelState + "=" + safeValue(inst.State),
			resourceLabelGPUType + "=" + safeValue(inst.GpuType),
			fmt.Sprintf("%s=%d", resourceLabelGPU, inst.GPU),
			fmt.Sprintf("%s=%d", resourceLabelCPU, inst.CPU),
			fmt.Sprintf("%s=%d", resourceLabelMemory, inst.Memory),
		}
		if inst.ImageType != "" {
			parts = append(parts, resourceLabelImageType+"="+safeValue(inst.ImageType))
		}
		if inst.StartTime != 0 {
			parts = append(parts, fmt.Sprintf("%s=%d", resourceLabelStartTime, inst.StartTime))
		}
		if inst.ExpireTime != 0 {
			parts = append(parts, fmt.Sprintf("%s=%d", resourceLabelExpireTime, inst.ExpireTime))
		}
		lines = append(lines, strings.Join(parts, ", "))
	}
	return strings.Join(lines, "\n")
}

func RenderMonitorSummary(metrics []Metric, payload map[string]any) string {
	redacted, _ := security.RedactForLLM(payload).(map[string]any)
	flat := map[string]string{}
	// Commit 3 must restrict this flattening to the real monitor response field
	// set before the handler is wired into engine cutover.
	flattenScalars("", redacted, flat)
	if len(flat) == 0 {
		return noMonitorValuesReply
	}

	keys := make([]string, 0, len(flat))
	for key := range flat {
		if len(metrics) == 0 || matchesRequestedMetric(key, metrics) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	if len(keys) == 0 {
		return noRequestedMonitorValuesReply
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
		// Demo skeleton intentionally uses substring matching; Commit 3 must
		// narrow this to known monitor response paths before production cutover.
		if strings.Contains(key, string(metric)) {
			return true
		}
	}
	return false
}

func safeValue(v any) string {
	return fmt.Sprint(security.RedactForLLM(v))
}
