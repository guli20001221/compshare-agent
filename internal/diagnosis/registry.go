package diagnosis

// registeredDiagnosisActions is the ADVERTISED diagnosis set: the tools exposed
// to the LLM/planner and rendered in the diagnosis selection card. GPU / image /
// port-firewall are intentionally NOT advertised — they are migrating to the
// in-instance SSH-ops harness, so their Go chains stay dormant (still resolvable
// in chainRegistry + unit-tested) but unreachable until that lands. Init-failure
// was removed outright (no diagnosis value: it resolves to 联系客服 / 删除重建).
var registeredDiagnosisActions = []string{
	"DiagnoseSSH",
	"DiagnoseBilling",
}

// chainRegistry maps a diagnosis action to its Go chain. It is deliberately a
// SUPERSET of registeredDiagnosisActions: the migrating GPU / image / port chains
// remain resolvable so their chains + skill-executor pilots stay exercised, even
// though they are no longer advertised as reachable tools.
var chainRegistry = map[string]func() *Chain{
	"DiagnoseSSH":            SSHFailureChain,
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
