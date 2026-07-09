package intent

import (
	"time"

	"github.com/compshare-agent/internal/observability"
)

type PlannerTraceOptions struct {
	Enabled bool
	Model   string
	Latency time.Duration
}

type PlannerTraceTargetRef struct {
	Type           string `json:"type"`
	Source         string `json:"source,omitempty"`
	ValueHash      string `json:"value_hash,omitempty"`
	SourceSpanHash string `json:"source_span_hash,omitempty"`
}

type PlannerTraceTimeWindow struct {
	Type      string `json:"type"`
	Value     string `json:"value,omitempty"`
	ValueHash string `json:"value_hash,omitempty"`
}

func ProjectPlannerTrace(result IntentRouterResult, opts PlannerTraceOptions) observability.RouterTrace {
	trace := observability.RouterTrace{
		Slots: observability.PlannerSlots{
			TargetRefs: []any{},
			Metrics:    []string{},
		},
	}
	if !opts.Enabled {
		return trace
	}

	trace.Enabled = true
	trace.Model = opts.Model
	trace.LatencyMS = opts.Latency.Milliseconds()
	trace.InputTokens = result.Usage.PromptTokens
	trace.OutputTokens = result.Usage.CompletionTokens
	trace.SchemaValid = !result.Fallback && result.Plan.SchemaVersion == SchemaVersion && result.Plan.Intent != ""
	trace.Intent = string(result.Plan.Intent)
	trace.PlannedExecutionPath = string(PlannedExecutionPathForIntent(result.Plan.Intent))
	trace.Skills = projectPlannerSkills(DeriveSelectedSkills(result.Plan))
	trace.Slots = projectPlannerSlots(result.Plan.Slots)
	trace.Confidence = result.Plan.Confidence
	trace.HardBlockHint = result.Plan.Intent == IntentBillingAccountUnsupported
	if !trace.SchemaValid {
		trace.Intent = string(IntentUnknown)
		trace.PlannedExecutionPath = ""
		trace.Skills = nil
		trace.Confidence = 0
		trace.HardBlockHint = false
	}
	return trace
}

func projectPlannerSkills(skills []SelectedSkill) []observability.PlannerSkillTrace {
	out := make([]observability.PlannerSkillTrace, 0, len(skills))
	for _, skill := range skills {
		out = append(out, observability.PlannerSkillTrace{
			Name:       skill.Name,
			Resolution: skill.Resolution,
		})
	}
	return out
}

func projectPlannerSlots(slots Slots) observability.PlannerSlots {
	out := observability.PlannerSlots{
		TargetRefs: make([]any, 0, len(slots.TargetRefs)),
		Metrics:    make([]string, 0, len(slots.Metrics)),

		ImageSource: string(slots.ImageSource),
		ListMode:    string(slots.ListMode),
		PriceKind:   string(slots.PriceKind),
		CFSKind:     string(slots.CFSKind),
		ChargeType:  slots.ChargeType,
		DetailLevel: string(slots.DetailLevel),
		SizeGB:      slots.SizeGB,

		SearchQueryHash: hashPlannerTraceValue(slots.SearchQuery),
		ZoneHash:        hashPlannerTraceValue(slots.Zone),
	}
	out.Action, out.ActionHash = projectPlannerLifecycleAction(slots.Action)
	for _, ref := range slots.TargetRefs {
		projected := PlannerTraceTargetRef{
			Type:      string(ref.Type),
			Source:    string(ref.Source),
			ValueHash: hashPlannerTraceValue(ref.Value),
		}
		if ref.SourceSpan != "" {
			projected.SourceSpanHash = hashPlannerTraceValue(ref.SourceSpan)
		}
		out.TargetRefs = append(out.TargetRefs, projected)
	}
	for _, metric := range slots.Metrics {
		out.Metrics = append(out.Metrics, string(metric))
	}
	if slots.TimeWindow != nil {
		out.TimeWindow = projectPlannerTimeWindow(*slots.TimeWindow)
	}
	return out
}

// projectPlannerLifecycleAction closes slots.action against the known verbs
// before it reaches a trace. The router schema deliberately omits slots.action
// and is non-strict, so a model that volunteers an arbitrary string still yields
// a SchemaValid plan, and the engine only re-derives the verb when the slot is
// EMPTY (see engine.tryOperationLifecycleDispatch) — a bogus non-empty value is
// never overwritten, only rejected later by lifecycleWorkflowName. Recording it
// verbatim would put raw model text in the trace. Known verb -> verbatim (it is
// the dispatch decision variable); anything else -> hashed, mirroring how a
// non-canonical TimeWindow value is handled.
func projectPlannerLifecycleAction(action LifecycleAction) (value, hash string) {
	if action == "" {
		return "", ""
	}
	if isPlannerTraceKnownLifecycleAction(action) {
		return string(action), ""
	}
	return "", hashPlannerTraceValue(string(action))
}

func isPlannerTraceKnownLifecycleAction(action LifecycleAction) bool {
	switch action {
	case LifecycleActionStop, LifecycleActionStart, LifecycleActionReboot,
		LifecycleActionReinstall, LifecycleActionResize, LifecycleActionResetPwd,
		LifecycleActionRename, LifecycleActionCreateDisk:
		return true
	default:
		return false
	}
}

func projectPlannerTimeWindow(window TimeWindow) PlannerTraceTimeWindow {
	out := PlannerTraceTimeWindow{Type: string(window.Type)}
	if isPlannerTraceCanonicalTimeWindow(window.Value) {
		out.Value = window.Value
		return out
	}
	out.ValueHash = hashPlannerTraceValue(window.Value)
	return out
}

func isPlannerTraceCanonicalTimeWindow(value string) bool {
	switch value {
	case "now", "today", "yesterday", "last_1h", "last_24h", "last_7d":
		return true
	default:
		return false
	}
}

func hashPlannerTraceValue(value string) string {
	if value == "" {
		return ""
	}
	hash, err := observability.HashTracePayload(value)
	if err != nil {
		return ""
	}
	return hash
}
