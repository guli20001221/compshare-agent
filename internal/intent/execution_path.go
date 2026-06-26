package intent

type ExecutionPath string

const (
	ExecutionPathRouting     ExecutionPath = "routing"
	ExecutionPathTerminalRAG ExecutionPath = "terminal_rag"
	ExecutionPathAgent       ExecutionPath = "agent"
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
		IntentPlatformImageList,
		IntentCustomImageList,
		IntentCommunityImageList,
		IntentSharedImageList,
		IntentPricingQuery:
		return ExecutionPathRouting
	case IntentKnowledgeQA:
		return ExecutionPathTerminalRAG
	default:
		return ExecutionPathAgent
	}
}

func IsExecutionPath(value ExecutionPath) bool {
	switch value {
	case ExecutionPathRouting, ExecutionPathTerminalRAG, ExecutionPathAgent:
		return true
	default:
		return false
	}
}
