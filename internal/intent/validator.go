package intent

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/compshare-agent/internal/entity"
)

const MaxSourceSpanRunes = 50

type ErrorCode string

const (
	ErrInvalidSchemaVersion        ErrorCode = "invalid_schema_version"
	ErrInvalidIntent               ErrorCode = "invalid_intent"
	ErrInvalidTargetRefType        ErrorCode = "invalid_target_ref_type"
	ErrInvalidMetric               ErrorCode = "invalid_metric"
	ErrInvalidTimeWindow           ErrorCode = "invalid_time_window"
	ErrInvalidRequiredTool         ErrorCode = "invalid_required_tool"
	ErrAttemptedHallucinatedEntity ErrorCode = "attempted_hallucinated_entity"
	ErrEntityNotFound              ErrorCode = "entity_not_found"
	ErrNameTooShort                ErrorCode = "name_too_short"
	ErrRetrievalDisabled           ErrorCode = "retrieval_disabled"
	ErrInvalidConfidence           ErrorCode = "invalid_confidence"
)

type ValidationError struct {
	Code  ErrorCode
	Field string
	Msg   string
}

func (e *ValidationError) Error() string {
	if e == nil {
		return ""
	}
	if e.Field == "" {
		return fmt.Sprintf("%s: %s", e.Code, e.Msg)
	}
	return fmt.Sprintf("%s at %s: %s", e.Code, e.Field, e.Msg)
}

type ValidationContext struct {
	UserText  string
	PriorText string
	Resolver  EntityResolver
	// Deprecated: use Resolver so shadow mode can validate against immutable
	// registry snapshots instead of the mutable EntityRegistry object.
	Registry *entity.EntityRegistry
}

type EntityResolver interface {
	ResolveByID(id string) (*entity.InstanceSnapshot, entity.ResolveResult)
	ResolveByName(name string) ([]*entity.InstanceSnapshot, entity.ResolveResult)
}

func ValidatePlan(plan Plan, ctx ValidationContext) error {
	if plan.SchemaVersion != SchemaVersion {
		return validationErr(ErrInvalidSchemaVersion, "schema_version", "unsupported schema version")
	}
	if !validIntent(plan.Intent) {
		return validationErr(ErrInvalidIntent, "intent", "unknown intent")
	}
	if plan.Confidence < 0 || plan.Confidence > 1 {
		return validationErr(ErrInvalidConfidence, "confidence", "confidence must be within [0,1]")
	}
	if plan.Retrieval.Enabled {
		return validationErr(ErrRetrievalDisabled, "retrieval.enabled", "RAG retrieval is disabled in stage 2A")
	}
	for i, tool := range plan.RequiredTools {
		if !validRequiredTool(tool) {
			return validationErr(ErrInvalidRequiredTool, fmt.Sprintf("required_tools[%d]", i), "unsupported required tool")
		}
		if !validRequiredToolForIntent(plan.Intent, tool) {
			return validationErr(ErrInvalidRequiredTool, fmt.Sprintf("required_tools[%d]", i), "required tool is not allowed for intent")
		}
	}
	if plan.Intent == IntentBillingAccountUnsupported && len(plan.RequiredTools) > 0 {
		return validationErr(ErrInvalidRequiredTool, "required_tools", "account-level billing unsupported intent must not call tools")
	}
	if plan.Intent == IntentBillingAccountUnsupported && len(plan.Slots.TargetRefs) > 0 {
		return validationErr(ErrInvalidTargetRefType, "slots.target_refs", "account-level billing unsupported intent must not carry instance target_refs")
	}
	if containsFilterRef(plan.Slots.TargetRefs) {
		if _, err := ParseResourceFilters(plan.Slots.TargetRefs); err != nil {
			return validationErr(ErrInvalidTargetRefType, "slots.target_refs", err.Error())
		}
	}
	for i, ref := range plan.Slots.TargetRefs {
		if err := validateTargetRef(ref, i, ctx); err != nil {
			return err
		}
	}
	for i, metric := range plan.Slots.Metrics {
		if !validMetric(metric) {
			return validationErr(ErrInvalidMetric, fmt.Sprintf("slots.metrics[%d]", i), "unsupported metric enum")
		}
	}
	if plan.Slots.TimeWindow != nil && !validTimeWindowType(plan.Slots.TimeWindow.Type) {
		return validationErr(ErrInvalidTimeWindow, "slots.time_window.type", "unsupported time_window type")
	}
	return nil
}

func validateTargetRef(ref TargetRef, idx int, ctx ValidationContext) error {
	field := fmt.Sprintf("slots.target_refs[%d]", idx)
	resolver := ctx.entityResolver()
	switch ref.Type {
	case TargetRefFilter:
		if !validFilterRef(ref.Value) {
			return validationErr(ErrInvalidTargetRefType, field+".value", "unsupported filter target_ref value")
		}
		return nil
	case TargetRefSlotPosition:
		if !validSlotPosition(ref.Value) {
			return validationErr(ErrInvalidTargetRefType, field+".value", "unsupported slot_position target_ref value")
		}
		return nil
	case TargetRefName:
		if utf8.RuneCountInString(strings.TrimSpace(ref.Value)) < 2 {
			return validationErr(ErrNameTooShort, field+".value", "name target_ref must be at least 2 characters")
		}
		if err := validateProvenance(ref, field, ctx); err != nil {
			return err
		}
		if resolver != nil {
			if matches, res := resolver.ResolveByName(ref.Value); res.Status == entity.ResolveNotFoundInAccount || len(matches) == 0 {
				return validationErr(ErrEntityNotFound, field+".value", "name target_ref does not resolve in registry")
			}
		}
		return nil
	case TargetRefUHostIDUserInput:
		if err := validateProvenance(ref, field, ctx); err != nil {
			return err
		}
		if resolver != nil {
			if _, res := resolver.ResolveByID(ref.Value); res.Status != entity.ResolveHit {
				return validationErr(ErrEntityNotFound, field+".value", "uhost_id target_ref is not in registry")
			}
		}
		return nil
	default:
		// C15 Phase A (PR #89, 2026-05-21): the new TargetRefZone /
		// TargetRefImage / TargetRefGPUModel constants are declared in
		// types.go but DELIBERATELY not accepted here. Phase A is a
		// pure type-taxonomy addition with strict zero runtime
		// behavior change — if the planner accidentally emits one of
		// the new types (it can't today, the prompt doesn't reference
		// them), this default branch rejects it just like any other
		// unknown type and the planner-result fallback path triggers
		// (consistent with pre-PR #89 behavior).
		//
		// Phase B will:
		//   - extend planner directives so the LLM emits these types
		//   - add Zone / Image / GPUModel resolvers in entity package
		//   - add a switch case here mirroring TargetRefUHostIDUserInput
		//     (provenance + resolver lookup) atomically with the
		//     planner-prompt SHA-256 hash bump (C5 contract).
		return validationErr(ErrInvalidTargetRefType, field+".type", "unsupported target_ref type")
	}
}

func (ctx ValidationContext) entityResolver() EntityResolver {
	if ctx.Resolver != nil {
		return ctx.Resolver
	}
	if ctx.Registry != nil {
		return ctx.Registry
	}
	return nil
}

func validateProvenance(ref TargetRef, field string, ctx ValidationContext) error {
	if ref.Source != SourceUserText && ref.Source != SourcePriorTurn {
		return validationErr(ErrAttemptedHallucinatedEntity, field+".source", "missing or invalid entity provenance source")
	}
	sourceSpan := strings.TrimSpace(ref.SourceSpan)
	if sourceSpan == "" || utf8.RuneCountInString(sourceSpan) > MaxSourceSpanRunes {
		return validationErr(ErrAttemptedHallucinatedEntity, field+".source_span", "missing or too long entity source_span")
	}
	haystack := ctx.UserText
	if ref.Source == SourcePriorTurn {
		haystack = ctx.PriorText
	}
	if !strings.Contains(haystack, sourceSpan) {
		return validationErr(ErrAttemptedHallucinatedEntity, field+".source_span", "source_span is not present in declared source text")
	}
	if ref.Type == TargetRefUHostIDUserInput && !strings.Contains(sourceSpan, ref.Value) {
		return validationErr(ErrAttemptedHallucinatedEntity, field+".value", "uhost_id value is not present in source_span")
	}
	return nil
}

func validIntent(intent Intent) bool {
	for _, allowed := range RuntimeIntents() {
		if intent == allowed {
			return true
		}
	}
	return false
}

func validMetric(metric Metric) bool {
	switch metric {
	case MetricCPU, MetricMemory, MetricGPU, MetricVRAM:
		return true
	default:
		return false
	}
}

func validTimeWindowType(windowType TimeWindowType) bool {
	switch windowType {
	case TimeWindowPreset, TimeWindowRelative, TimeWindowAbsolute:
		return true
	default:
		return false
	}
}

func validRequiredTool(tool string) bool {
	switch tool {
	case "DescribeCompShareInstance",
		"GetCompShareInstanceMonitor",
		"DiagnoseBilling",
		"GetCompShareInstancePrice",
		"GetCompShareInstanceUserPrice",
		// Capability Registry v1 (PR A, 2026-05-18). Keep the legacy list above
		// untouched and accept the 4 capability-bound platform-query tools.
		"DescribeAvailableCompShareInstanceTypes",
		"CheckCompShareResourceCapacity",
		"DescribeCompShareImages",
		"DescribeCompShareCustomImages",
		"DescribeCommunityImages":
		return true
	default:
		return false
	}
}

func validRequiredToolForIntent(intent Intent, tool string) bool {
	allowed := requiredToolsForIntent(intent)
	_, ok := allowed[tool]
	return ok
}

func requiredToolsForIntent(intent Intent) map[string]struct{} {
	allowed := map[string]struct{}{}
	add := func(actions ...string) {
		for _, action := range actions {
			allowed[action] = struct{}{}
		}
	}

	switch intent {
	case IntentResourceInfo:
		add("DescribeCompShareInstance")
	case IntentMonitorQuery:
		add("DescribeCompShareInstance", "GetCompShareInstanceMonitor")
	case IntentBillingInstance:
		add("DescribeCompShareInstance", "DiagnoseBilling")
	case IntentDiagnosis:
		add("DescribeCompShareInstance")
	case IntentOperationLifecycle:
		// Read tool ONLY. The planner resolves/lists the target via
		// DescribeCompShareInstance; the actual mutation runs through the
		// workflow layer + confirm (still gated by COMPSHARE_ENABLE_MUTATING_TOOLS),
		// never via plan.required_tools — so do NOT add *Workflow / mutating
		// actions here even though IntentToolSubset lists them. The few-shots
		// teach exactly this one tool (planner.go); without this case every
		// operation_lifecycle plan failed validation → fallback_invalid → unknown.
		add("DescribeCompShareInstance")
	case IntentDiskInfo:
		// Read tool ONLY. Disk facts surface solely via DiskSet[] in the
		// DescribeCompShareInstance response (no disk-list API upstream;
		// 2026-05-29 routing fix). Same drift as operation_lifecycle above.
		add("DescribeCompShareInstance")
	case IntentDeployModel:
		// Read tools ONLY (the image-catalog grounding reads the deploy arm uses
		// to match a workload to an existing image). The actual mutation
		// (CreateCompShareInstance) runs through the orchestrator saga + StepConfirm,
		// never via plan.required_tools — same discipline as operation_lifecycle.
		// The arm ignores plan.required_tools; this case only keeps the few-shots'
		// declared tool accepted by ValidatePlan (B8.3 ③).
		add("DescribeCompShareImages", "DescribeCommunityImages")
	}

	if required, ok := capabilityRequiredTool(intent); ok {
		add(required)
		for _, action := range extraHandlerActions()[intent] {
			add(action)
		}
	}

	return allowed
}

func validFilterRef(value string) bool {
	_, err := ParseResourceFilter(value)
	return err == nil
}

func containsFilterRef(refs []TargetRef) bool {
	for _, ref := range refs {
		if ref.Type == TargetRefFilter {
			return true
		}
	}
	return false
}

func validSlotPosition(value string) bool {
	switch value {
	case "first_running", "last_mentioned":
		return true
	default:
		return false
	}
}

func validationErr(code ErrorCode, field, msg string) *ValidationError {
	return &ValidationError{Code: code, Field: field, Msg: msg}
}
