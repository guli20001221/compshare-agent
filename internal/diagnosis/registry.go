package diagnosis

var registeredDiagnosisActions = []string{
	"DiagnoseSSH",
	"DiagnoseInitFailure",
	"DiagnoseGPU",
	"DiagnoseBilling",
	"DiagnosePortOrFirewall",
	"DiagnoseImageIssue",
}

var chainRegistry = map[string]func() *Chain{
	"DiagnoseSSH":            SSHFailureChain,
	"DiagnoseInitFailure":    InitFailureChain,
	"DiagnoseGPU":            GPUNotDetectedChain,
	"DiagnoseBilling":        BillingAnomalyChain,
	"DiagnosePortOrFirewall": PortFirewallChain,
	"DiagnoseImageIssue":     ImageIssueChain,
}

// RegisteredDiagnosisActions returns diagnosis action names in prompt-stable
// human order.
func RegisteredDiagnosisActions() []string {
	return append([]string(nil), registeredDiagnosisActions...)
}

func IsDiagnosisTool(action string) bool {
	_, ok := chainRegistry[action]
	return ok
}

func GetChain(action string) (*Chain, bool) {
	factory, ok := chainRegistry[action]
	if !ok {
		return nil, false
	}
	return factory(), true
}
