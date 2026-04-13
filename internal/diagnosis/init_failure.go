package diagnosis

func InitFailureChain() *Chain {
	return &Chain{Name: "DiagnoseInitFailure", Steps: []Step{}, Fallback: Verdict{Action: Conclude, Conclusion: "placeholder"}}
}
