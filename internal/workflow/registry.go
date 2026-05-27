package workflow

// workflowRegistry maps workflow action names to their factory functions.
var workflowRegistry = map[string]func() *Definition{
	"CreateInstanceWorkflow":      CreateInstanceDef,
	"StopInstanceWorkflow":        StopInstanceDef,
	"StartInstanceWorkflow":       StartInstanceDef,
	"RebootInstanceWorkflow":      RebootInstanceDef,
	"RenameInstanceWorkflow":      RenameInstanceDef,
	"ResetPasswordWorkflow":       ResetPasswordDef,
	"SetStopSchedulerWorkflow":    SetStopSchedulerDef,
	"CancelStopSchedulerWorkflow": CancelStopSchedulerDef,
	"ResizeInstanceWorkflow":      ResizeInstanceDef,
	"ReinstallInstanceWorkflow":   ReinstallInstanceDef,
	"CreateDiskWorkflow":          CreateDiskDef,
}

// IsWorkflowTool reports whether the given action name corresponds to a
// registered workflow.
func IsWorkflowTool(action string) bool {
	_, ok := workflowRegistry[action]
	return ok
}

// GetWorkflow returns a fresh Definition for the named workflow. The second
// return value is false if no workflow is registered under that name.
func GetWorkflow(action string) (*Definition, bool) {
	factory, ok := workflowRegistry[action]
	if !ok {
		return nil, false
	}
	return factory(), true
}
