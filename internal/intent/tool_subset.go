package intent

// IntentToolSubset returns the tool names to expose when an intent falls back
// to ReAct. Returns nil for intents that should see the full tool set or are
// handled outside the shared ReAct loop.
func IntentToolSubset(i Intent) []string {
	switch i {
	case IntentDiagnosis, IntentVagueFailure:
		return []string{
			// Generic diagnosis ReAct does not receive SearchKnowledge: its final
			// prose mixes API results with KB facts and has no unified proof for
			// both. Diagnosis skills that require KB evidence use the orchestrator's
			// virtual SearchKnowledge path and validate structured diagnosis_claims.
			"DiagnoseSSH",
			"DiagnoseBilling",
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
	case IntentBillingInstance:
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
