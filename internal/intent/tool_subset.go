package intent

// IntentToolSubset returns the tool names to expose when an intent falls back
// to ReAct. Returns nil for intents that should see the full tool set (unknown,
// knowledge_qa, mixed intents, etc.).
func IntentToolSubset(i Intent) []string {
	switch i {
	case IntentDiagnosis, IntentVagueFailure:
		return []string{
			"DiagnoseSSH",
			"DiagnoseInitFailure",
			"DiagnoseGPU",
			"DiagnoseBilling",
			"DiagnosePortOrFirewall",
			"DiagnoseImageIssue",
			"DescribeCompShareInstance",
			"GetCompShareInstanceMonitor",
			"DescribeCompShareSoftwarePort",
		}
	case IntentResourceInfo:
		return []string{
			"DescribeCompShareInstance",
			"GetCompShareInstanceMonitor",
			"DescribeCompShareSoftwarePort",
			"DescribeCompShareJupyterToken",
			"GetCompShareInstanceUserPrice",
		}
	case IntentMonitorQuery:
		return []string{
			"DescribeCompShareInstance",
			"GetCompShareInstanceMonitor",
		}
	case IntentBillingInstance, IntentExpiryRenewal:
		return []string{
			"DescribeCompShareInstance",
			"GetCompShareInstanceUserPrice",
			"GetCompShareInstancePrice",
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
			"CheckCompShareResourceCapacity",
			"DescribeCompShareImages",
		}
	case IntentPricingQuery:
		return []string{
			"GetCompShareInstancePrice",
			"DescribeAvailableCompShareInstanceTypes",
		}
	case IntentPlatformImageList:
		return []string{
			"DescribeCompShareImages",
		}
	case IntentCustomImageList:
		return []string{
			"DescribeCompShareCustomImages",
		}
	case IntentCommunityImageList:
		return []string{
			"DescribeCommunityImages",
		}
	case IntentRecommendation:
		return []string{
			"GetGPURecommendation",
			"DescribeAvailableCompShareInstanceTypes",
			"GetCompShareInstancePrice",
			"DescribeCompShareImages",
		}
	case IntentOperationLifecycle:
		return []string{
			"DescribeCompShareInstance",
			"DescribeAvailableCompShareInstanceTypes",
			"DescribeCompShareImages",
			"GetCompShareInstancePrice",
			"CheckCompShareResourceCapacity",
			"CreateInstanceWorkflow",
			"StopInstanceWorkflow",
			"StartInstanceWorkflow",
			"RebootInstanceWorkflow",
			"RenameInstanceWorkflow",
			"ResetPasswordWorkflow",
			"SetStopSchedulerWorkflow",
			"CancelStopSchedulerWorkflow",
		}
	default:
		return nil
	}
}
