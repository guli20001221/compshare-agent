package diagnosis

// registeredDiagnosisActions is the diagnosis set exposed to the LLM/planner and
// rendered in the diagnosis selection card. Init-failure was removed (no diagnosis
// value: it resolves to 联系客服 / 删除重建); the GPU / image / port-firewall chains
// were DELETED outright — they had been parked as dormant-but-resolvable "for a
// future SSH-ops harness", but keeping an unadvertised tool resolvable relies on the
// model never naming it, which is not a safety boundary. A future in-instance
// SSH-ops capability must be built in the typed-capability architecture, not
// resurrected as a legacy chain here.
var registeredDiagnosisActions = []string{
	"DiagnoseBilling",
}

// chainRegistry maps a diagnosis action to its Go chain. It MUST stay equal to
// registeredDiagnosisActions — no dormant/unadvertised superset — so an unadvertised
// diagnosis name can never resolve to an executable chain (enforced by
// TestDiagnosisRegistryHasNoUnadvertisedChains).
var chainRegistry = map[string]func() *Chain{
	"DiagnoseBilling": BillingAnomalyChain,
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
