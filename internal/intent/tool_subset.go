package intent

// IntentToolSubset returns the tool names to expose when an intent falls back
// to ReAct. Returns nil for intents that should see the full tool set (unknown,
// capability intents that rarely fall back, etc.).
//
// Only diagnosis benefits today: the planner classifies the intent but the
// handler can only do clarification; ReAct takes over with a scoped tool set
// instead of all 19 read-only tools. This reduces tool-selection noise.
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
	default:
		return nil
	}
}
