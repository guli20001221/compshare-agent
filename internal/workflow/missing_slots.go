package workflow

import "strings"

// MissingSlotsForFailure maps workflow-owned "missing parameter" failures to
// generic task slots. The engine uses this to keep a pending task alive without
// embedding per-phrase continuation rules.
func MissingSlotsForFailure(workflowName, message string) []string {
	msg := strings.TrimSpace(message)
	switch workflowName {
	case "CreateDiskWorkflow":
		if strings.Contains(msg, createDiskMissingSizeMessage) {
			return []string{"size_gb"}
		}
	case "ResizeDiskWorkflow":
		if strings.Contains(msg, resizeDiskMissingTargetMessage) {
			return []string{"target_size_gb"}
		}
	case "ResizeInstanceWorkflow":
		if strings.Contains(msg, resizeInstanceMissingSpecMessage) {
			return []string{"cpu", "memory_gb", "gpu_count"}
		}
	}
	return nil
}
