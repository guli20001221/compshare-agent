package intent

type ExecutionPath string

const (
	ExecutionPathRouting ExecutionPath = "routing"
	ExecutionPathAgent   ExecutionPath = "agent"
)

func PlannedExecutionPathForIntent(i Intent) ExecutionPath {
	switch i {
	case IntentMonitorQuery,
		IntentBillingAccountUnsupported,
		IntentResourceInfo,
		IntentGPUSpecsQuery,
		IntentStockAvailability,
		IntentNetAcceleratorStatus,
		IntentRefundEstimate,
		IntentCFSInfo,
		IntentImageTagCatalog,
		IntentModelRepositoryBrowse,
		IntentImageList,
		IntentPricingQuery:
		return ExecutionPathRouting
	default:
		return ExecutionPathAgent
	}
}

func IsExecutionPath(value ExecutionPath) bool {
	switch value {
	case ExecutionPathRouting, ExecutionPathAgent:
		return true
	default:
		return false
	}
}
