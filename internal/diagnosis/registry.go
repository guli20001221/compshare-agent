package diagnosis

// registeredDiagnosisActions is the complete set exposed to the model and the
// diagnosis selection card. Resolvable and advertised actions must stay equal.
var registeredDiagnosisActions = []string{
	"DiagnoseBilling",
}

// chainRegistry maps each advertised diagnosis action to its Go chain.
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
