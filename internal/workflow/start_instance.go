package workflow

// StartInstanceDef returns the 2-step workflow definition for starting a
// CompShare GPU instance: confirm, then start.
func StartInstanceDef() *Definition {
	return &Definition{
		Name:        "StartInstanceWorkflow",
		Description: "确认开机 → 开机",
		Steps: []Step{
			stepConfirmStart(),
			stepStartInstance(),
		},
	}
}

// ---------------------------------------------------------------------------
// Step definitions
// ---------------------------------------------------------------------------

func stepConfirmStart() Step {
	return Step{
		Name: "确认开机",
		Type: StepConfirm,
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			return map[string]any{
				"workflow": "StartInstanceWorkflow",
				"UHostId":  wfCtx.Params["UHostId"],
			}, nil
		},
	}
}

func stepStartInstance() Step {
	return Step{
		Name: "开机",
		Type: StepToolCall,
		Tool: "StartCompShareInstance",
		// Zone + UHostId required per docs/api/instance/StartCompShareInstance.md
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			return map[string]any{
				"Zone":    paramStr(wfCtx.Params, "Zone", defaultZone),
				"UHostId": wfCtx.Params["UHostId"],
			}, nil
		},
	}
}
