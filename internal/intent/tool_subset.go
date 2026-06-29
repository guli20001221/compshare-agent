package intent

// IntentToolSubset returns the tool names to expose when an intent falls back
// to ReAct. Returns nil for intents that should see the full tool set or are
// handled outside the shared ReAct loop.
func IntentToolSubset(i Intent) []string {
	switch i {
	case IntentDiagnosis, IntentVagueFailure:
		return []string{
			// SearchKnowledge (agentic-RAG, P3/P4a) is a CANDIDATE diagnosis tool
			// so the agent can retrieve prior tool/ops evidence BEFORE any
			// Diagnose* tool. It is emit-gated at the visibility layer
			// (tools.VisibleRegistry drops it unless COMPSHARE_AGENTIC_SEARCH_KNOWLEDGE
			// is on), so the EMITTED diagnosis subset is byte-identical to before
			// when the flag is off. Listing it here is inert until the flag is on.
			"SearchKnowledge",
			"DiagnoseSSH",
			"DiagnoseInitFailure",
			"DiagnoseGPU",
			"DiagnoseBilling",
			"DiagnosePortOrFirewall",
			"DiagnoseImageIssue",
			"DescribeCompShareInstance",
			"GetCompShareInstanceMonitor",
			"DescribeCompShareSoftwarePort",
			"DescribeCompShareJupyterToken",
		}
	case IntentResourceInfo:
		return []string{
			"DescribeCompShareInstance",
			"GetCompShareInstanceMonitor",
			"DescribeCompShareSoftwarePort",
			"DescribeCompShareJupyterToken",
			"DescribeCFS",
			"GetCompShareInstanceUserPrice",
		}
	case IntentDiskInfo:
		return []string{
			"DescribeCompShareInstance",
		}
	case IntentMonitorQuery, IntentMonitorHistory:
		return []string{
			"DescribeCompShareInstance",
			"GetCompShareInstanceMonitor",
		}
	case IntentBillingInstance, IntentExpiryRenewal:
		return []string{
			"DescribeCompShareInstance",
			"GetCompShareInstanceUserPrice",
			"GetCompShareInstancePrice",
			"GetCompShareRefundPrice",
			"GetCompShareCFSRefundPrice",
			"DiagnoseBilling",
		}
	case IntentGPUSpecsQuery:
		return []string{
			"DescribeAvailableCompShareInstanceTypes",
			"GetGPUSpecs",
		}
	case IntentStockAvailability:
		return []string{
			"DescribeAvailableCompShareInstanceTypes",
			"DescribeCompShareSupportZone",
			"DescribeCompShareGpuInventory",
			"CheckCompShareResourceCapacity",
			"DescribeCompShareImages",
		}
	case IntentPricingQuery:
		return []string{
			"GetCompShareInstanceUserPrice",
			"DescribeCompShareSupportZone",
			"DescribeAvailableCompShareInstanceTypes",
		}
	case IntentNetAcceleratorStatus:
		return []string{
			"CheckCompShareNetOptimizer",
		}
	case IntentRefundEstimate:
		return []string{
			"DescribeCompShareInstance",
			"GetCompShareRefundPrice",
		}
	case IntentCFSInfo:
		return []string{
			"DescribeCFS",
			"GetCompShareCFSPrice",
			"GetCompShareCFSUpgradePrice",
			"GetCompShareCFSRefundPrice",
		}
	case IntentImageTagCatalog:
		return []string{
			"DescribeCompShareImageTags",
		}
	case IntentModelRepositoryBrowse:
		return []string{
			"DescribeModelRepositoryModels",
			"DescribeModelRepositoryTags",
		}
	case IntentImageList:
		return []string{
			"DescribeCompShareImages",
			"DescribeCompShareCustomImages",
			"DescribeCommunityImages",
			"DescribeCompShareSharingImages",
		}
	case IntentOperationLifecycle:
		return []string{
			"DescribeCompShareInstance",
			"DescribeAvailableCompShareInstanceTypes",
			"DescribeCompShareImages",
			"DescribeCommunityImages",
			"GetCompShareInstancePrice",
			"GetCompShareInstanceUpgradePrice",
			"CheckCompShareResourceCapacity",
			"StopInstanceWorkflow",
			"StartInstanceWorkflow",
			"RebootInstanceWorkflow",
			"RenameInstanceWorkflow",
			"ResetPasswordWorkflow",
			"SetStopSchedulerWorkflow",
			"CancelStopSchedulerWorkflow",
			"ResizeInstanceWorkflow",
			"ResizeDiskWorkflow",
			"ReinstallInstanceWorkflow",
			"CreateDiskWorkflow",
			"CreateCustomImageWorkflow",
			"EnableNetOptimizerWorkflow",
			"CreateCFSWorkflow",
			"ResizeCFSWorkflow",
		}
	default:
		return nil
	}
}
