package intent

type RuntimeForm string

const (
	RuntimeFormRouting     RuntimeForm = "routing"
	RuntimeFormTerminalRAG RuntimeForm = "terminal_rag"
	RuntimeFormAgent       RuntimeForm = "agent"
)

func PlannedRuntimeFormForIntent(i Intent) RuntimeForm {
	switch i {
	case IntentMonitorQuery,
		IntentResourceInfo,
		IntentGPUSpecsQuery,
		IntentStockAvailability,
		IntentNetAcceleratorStatus,
		IntentImageTagCatalog,
		IntentPlatformImageList,
		IntentCustomImageList,
		IntentCommunityImageList,
		IntentPricingQuery:
		return RuntimeFormRouting
	case IntentKnowledgeQA:
		return RuntimeFormTerminalRAG
	default:
		return RuntimeFormAgent
	}
}

func IsRuntimeForm(value RuntimeForm) bool {
	switch value {
	case RuntimeFormRouting, RuntimeFormTerminalRAG, RuntimeFormAgent:
		return true
	default:
		return false
	}
}
