package intent

type minimalPlanCore struct {
	Intent     Intent          `json:"intent"`
	TargetRefs []TargetRef     `json:"target_refs,omitempty"`
	Metrics    []Metric        `json:"metrics,omitempty"`
	TimeWindow *TimeWindow     `json:"time_window,omitempty"`
	Action     LifecycleAction `json:"action,omitempty"`
}

func compileMinimalPlanCore(core minimalPlanCore) IntentRoute {
	plan := IntentRoute{
		SchemaVersion: SchemaVersion,
		Intent:        core.Intent,
		Slots: Slots{
			TargetRefs: cloneTargetRefs(core.TargetRefs),
			Metrics:    append([]Metric(nil), core.Metrics...),
			TimeWindow: cloneTimeWindow(core.TimeWindow),
			Action:     core.Action,
		},
		Confidence: defaultMinimalCoreConfidence(core.Intent),
	}
	return withDerivedSelectedSkills(plan)
}

func defaultMinimalCoreConfidence(i Intent) float64 {
	switch i {
	case IntentUnknown:
		return 0.7
	default:
		return 0.85
	}
}

func cloneTargetRefs(in []TargetRef) []TargetRef {
	if len(in) == 0 {
		return nil
	}
	out := make([]TargetRef, len(in))
	copy(out, in)
	return out
}

func cloneTimeWindow(in *TimeWindow) *TimeWindow {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}
