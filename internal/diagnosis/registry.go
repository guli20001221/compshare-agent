package diagnosis

// chainRegistry maps each diagnosis action to its Go chain. The model-visible
// advertisement of these actions lives in tools.Registry; a chain that is
// resolvable here but absent there would be reachable only by a model guessing
// an unadvertised name.
var chainRegistry = map[string]func() *Chain{
	"DiagnoseBilling": BillingAnomalyChain,
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
