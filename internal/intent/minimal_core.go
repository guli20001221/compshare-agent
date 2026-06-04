package intent

import "sort"

type minimalPlanCore struct {
	Intent     Intent          `json:"intent"`
	TargetRefs []TargetRef     `json:"target_refs,omitempty"`
	Metrics    []Metric        `json:"metrics,omitempty"`
	TimeWindow *TimeWindow     `json:"time_window,omitempty"`
	Action     LifecycleAction `json:"action,omitempty"`
}

func compileMinimalPlanCore(core minimalPlanCore) Plan {
	plan := Plan{
		SchemaVersion: SchemaVersion,
		Intent:        core.Intent,
		Slots: Slots{
			TargetRefs: cloneTargetRefs(core.TargetRefs),
			Metrics:    append([]Metric(nil), core.Metrics...),
			TimeWindow: cloneTimeWindow(core.TimeWindow),
			Action:     core.Action,
		},
		RequiredTools: requiredToolsForIntentSorted(core.Intent),
		Retrieval:     Retrieval{Enabled: false},
		HardBlockHint: core.Intent == IntentBillingAccountUnsupported,
		Confidence:    defaultMinimalCoreConfidence(core.Intent),
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

func requiredToolsForIntentSorted(i Intent) []string {
	allowed := requiredToolsForIntent(i)
	out := make([]string, 0, len(allowed))
	for tool := range allowed {
		out = append(out, tool)
	}
	sort.Strings(out)
	return out
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
