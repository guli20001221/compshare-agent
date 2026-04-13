package diagnosis

var chainRegistry = map[string]func() *Chain{
	"DiagnoseSSH":         SSHFailureChain,
	"DiagnoseInitFailure": InitFailureChain,
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
