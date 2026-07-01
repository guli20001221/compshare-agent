package workflow

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// MissingSlotError carries workflow-owned missing parameter information without
// making the engine parse user-facing text.
type MissingSlotError struct {
	Message string
	Slots   []string
}

func (e MissingSlotError) Error() string { return e.Message }

func NewMissingSlotError(message string, slots ...string) error {
	return MissingSlotError{Message: strings.TrimSpace(message), Slots: uniqueSlotNames(slots)}
}

func MissingSlotsFromError(err error) []string {
	var missing MissingSlotError
	if errors.As(err, &missing) {
		return uniqueSlotNames(missing.Slots)
	}
	return nil
}

// MissingSlotsForFailure is retained only as a compatibility fallback for
// workflows that have not yet returned MissingSlotError.
func MissingSlotsForFailure(workflowName, message string) []string {
	if spec, ok := taskSlotSpecs[workflowName]; ok && spec.legacyMessage != "" && strings.Contains(strings.TrimSpace(message), spec.legacyMessage) {
		return append([]string(nil), spec.missingSlots...)
	}
	return nil
}

type taskSlotSpec struct {
	missingSlots  []string
	legacyMessage string
	parseUpdates  func(missing []string, userMsg string) map[string]string
	buildArgs     func(slots map[string]string) (map[string]any, []string)
	clarify       func(missing []string) string
}

var taskSlotSpecs = map[string]taskSlotSpec{
	"CreateDiskWorkflow": {
		missingSlots:  []string{"size_gb"},
		legacyMessage: createDiskMissingSizeMessage,
		parseUpdates: func(missing []string, userMsg string) map[string]string {
			if !slotSet(missing)["size_gb"] {
				return nil
			}
			if _, ok := parseTaskSlotNumber(userMsg); !ok {
				return nil
			}
			return map[string]string{"size_gb": strings.TrimSpace(userMsg)}
		},
		buildArgs: func(slots map[string]string) (map[string]any, []string) {
			normalized := NormalizeTaskSlotUpdates(slots)
			args, missing := baseWorkflowTaskArgs("CreateDiskWorkflow", normalized)
			if size, ok := parseTaskSlotNumber(normalized["size_gb"]); ok {
				args["Size"] = size
			} else {
				missing = append(missing, "size_gb")
			}
			return finishTaskArgs(args, missing)
		},
		clarify: func(missing []string) string {
			if slotSet(missing)["size_gb"] {
				return "需要先确认数据盘大小。请告诉我要加多大的数据盘，例如 200GB。"
			}
			return ""
		},
	},
	"ResizeDiskWorkflow": {
		missingSlots:  []string{"target_size_gb"},
		legacyMessage: resizeDiskMissingTargetMessage,
		parseUpdates: func(missing []string, userMsg string) map[string]string {
			if !slotSet(missing)["target_size_gb"] {
				return nil
			}
			if _, ok := parseTaskSlotNumber(userMsg); !ok {
				return nil
			}
			return map[string]string{"target_size_gb": strings.TrimSpace(userMsg)}
		},
		buildArgs: func(slots map[string]string) (map[string]any, []string) {
			normalized := NormalizeTaskSlotUpdates(slots)
			args, missing := baseWorkflowTaskArgs("ResizeDiskWorkflow", normalized)
			if size, ok := parseTaskSlotNumber(normalized["target_size_gb"]); ok {
				args["Size"] = size
			} else {
				missing = append(missing, "target_size_gb")
			}
			return finishTaskArgs(args, missing)
		},
		clarify: func(missing []string) string {
			if slotSet(missing)["target_size_gb"] {
				return "需要先确认扩容后的目标容量。请告诉我要扩到多少 GB，例如 200GB。"
			}
			return ""
		},
	},
	"ResizeInstanceWorkflow": {
		missingSlots:  []string{"cpu", "memory_gb", "gpu_count"},
		legacyMessage: resizeInstanceMissingSpecMessage,
		parseUpdates: func(missing []string, userMsg string) map[string]string {
			updates := map[string]string{}
			missingSet := slotSet(missing)
			for key, value := range resizeSpecUpdatesFromUserText(userMsg) {
				if missingSet[key] {
					updates[key] = value
				}
			}
			if len(updates) == 0 {
				return nil
			}
			return updates
		},
		buildArgs: func(slots map[string]string) (map[string]any, []string) {
			normalized := NormalizeTaskSlotUpdates(slots)
			args, missing := baseWorkflowTaskArgs("ResizeInstanceWorkflow", normalized)
			hasSpec := false
			if cpu, ok := parseTaskSlotNumber(normalized["cpu"]); ok {
				args["Cpu"] = cpu
				hasSpec = true
			}
			if mem, ok := parseTaskSlotNumber(normalized["memory_gb"]); ok {
				args["Memory"] = mem
				hasSpec = true
			}
			if gpu, ok := parseTaskSlotNumber(normalized["gpu_count"]); ok {
				args["Gpu"] = gpu
				hasSpec = true
			}
			if !hasSpec {
				missing = append(missing, "cpu", "memory_gb", "gpu_count")
			}
			return finishTaskArgs(args, missing)
		},
		clarify: func(missing []string) string {
			seen := slotSet(missing)
			if seen["cpu"] || seen["memory_gb"] || seen["gpu_count"] {
				return "需要先确认目标配置。请告诉我要改成多少 CPU、内存或 GPU，例如 4C8G。"
			}
			return ""
		},
	},
}

func TaskSlotUpdatesFromUserText(workflowName string, missing []string, userMsg string) map[string]string {
	spec, ok := taskSlotSpecs[workflowName]
	if !ok || spec.parseUpdates == nil {
		return nil
	}
	return NormalizeTaskSlotUpdates(spec.parseUpdates(missing, userMsg))
}

func TaskArgsFromSlots(workflowName string, slots map[string]string) (map[string]any, []string) {
	spec, ok := taskSlotSpecs[workflowName]
	if !ok || spec.buildArgs == nil {
		return nil, nil
	}
	return spec.buildArgs(slots)
}

func TaskMissingSlotsClarification(workflowName string, missing []string) string {
	if spec, ok := taskSlotSpecs[workflowName]; ok && spec.clarify != nil {
		if msg := spec.clarify(missing); msg != "" {
			return msg
		}
	}
	return "还缺少必要参数，无法安全继续。请补充后我再为你确认。"
}

func NormalizeTaskSlotUpdates(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		k := normalizeTaskSlotKey(key)
		v := strings.TrimSpace(value)
		if k == "" || v == "" {
			continue
		}
		out[k] = v
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func normalizeTaskSlotKey(key string) string {
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "instance_id", "uhost_id", "uhostid":
		return "instance_id"
	case "disk_id", "udisk_id", "udiskid":
		return "disk_id"
	case "size", "size_gb", "disk_size", "disk_size_gb":
		return "size_gb"
	case "target_size", "target_size_gb", "disk_space", "diskspace":
		return "target_size_gb"
	case "cpu", "cpu_count":
		return "cpu"
	case "memory", "memory_gb", "mem":
		return "memory_gb"
	case "gpu_count", "gpu_num":
		return "gpu_count"
	default:
		return ""
	}
}

func baseWorkflowTaskArgs(workflowName string, normalized map[string]string) (map[string]any, []string) {
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
	return args, missing
}

func finishTaskArgs(args map[string]any, missing []string) (map[string]any, []string) {
	if len(missing) > 0 {
		return map[string]any{}, uniqueSlotNames(missing)
	}
	return args, nil
}

func workflowRequiresInstanceTarget(workflowName string) bool {
	switch workflowName {
	case "CreateDiskWorkflow", "ResizeDiskWorkflow", "ResizeInstanceWorkflow":
		return true
	default:
		return false
	}
}

func parseTaskSlotNumber(value string) (float64, bool) {
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

var (
	taskResizeCPUMemorySpecRE = regexp.MustCompile(`(?i)(\d+)\s*(?:c|cpu|vcpu|核)\s*[/,， ]*\s*(\d+)\s*(?:g|gb|gib)`)
	taskResizeGPUCountSpecRE  = regexp.MustCompile(`(?i)(\d+)\s*(?:张\s*)?(?:gpu|卡)`)
)

const maxTaskResizeGPUCount = 16

func resizeSpecUpdatesFromUserText(userText string) map[string]string {
	updates := map[string]string{}
	for key, value := range ResizeWorkflowArgsFromUserText(userText) {
		switch key {
		case "Cpu":
			updates["cpu"] = contextTaskSlotString(value)
		case "Memory":
			updates["memory_gb"] = contextTaskSlotString(value)
		case "Gpu":
			updates["gpu_count"] = contextTaskSlotString(value)
		}
	}
	return updates
}

func ResizeWorkflowArgsFromUserText(userText string) map[string]any {
	args := map[string]any{}
	if m := taskResizeCPUMemorySpecRE.FindStringSubmatch(userText); len(m) == 3 {
		if cpu, ok := parseTaskSlotNumber(m[1]); ok && cpu > 0 {
			args["Cpu"] = cpu
		}
		if memGB, ok := parseTaskSlotNumber(m[2]); ok && memGB > 0 {
			args["Memory"] = memGB * 1024
		}
	}
	if m := taskResizeGPUCountSpecRE.FindStringSubmatch(userText); len(m) == 2 {
		if gpu, ok := parseTaskSlotNumber(m[1]); ok && gpu >= 0 && gpu <= maxTaskResizeGPUCount {
			args["Gpu"] = gpu
		}
	}
	return args
}

func formatTaskSlotFloat(v float64) string {
	if v == float64(int64(v)) {
		return strconv.FormatInt(int64(v), 10)
	}
	return strconv.FormatFloat(v, 'f', -1, 64)
}

func contextTaskSlotString(value any) string {
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
		return formatTaskSlotFloat(v)
	case float32:
		return formatTaskSlotFloat(float64(v))
	default:
		return strings.TrimSpace(fmt.Sprint(v))
	}
}

func slotSet(slots []string) map[string]bool {
	out := map[string]bool{}
	for _, slot := range slots {
		if normalized := normalizeTaskSlotKey(slot); normalized != "" {
			out[normalized] = true
		}
	}
	return out
}

func uniqueSlotNames(slots []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(slots))
	for _, slot := range slots {
		normalized := normalizeTaskSlotKey(slot)
		if normalized == "" || seen[normalized] {
			continue
		}
		seen[normalized] = true
		out = append(out, normalized)
	}
	return out
}
