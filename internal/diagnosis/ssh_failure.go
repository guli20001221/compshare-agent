package diagnosis

func SSHFailureChain() *Chain {
	return &Chain{Name: "DiagnoseSSH", Steps: []Step{}, Fallback: Verdict{Action: Conclude, Conclusion: "placeholder"}}
}
