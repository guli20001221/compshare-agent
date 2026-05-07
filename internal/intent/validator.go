package intent

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/compshare-agent/internal/entity"
)

type ErrorCode string

const (
	ErrInvalidSchemaVersion        ErrorCode = "invalid_schema_version"
	ErrInvalidIntent               ErrorCode = "invalid_intent"
	ErrInvalidTargetRefType        ErrorCode = "invalid_target_ref_type"
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
	Registry  *entity.EntityRegistry
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
	}
	if plan.Intent == IntentBillingAccountUnsupported && len(plan.RequiredTools) > 0 {
		return validationErr(ErrInvalidRequiredTool, "required_tools", "account-level billing unsupported intent must not call tools")
	}
	for i, ref := range plan.Slots.TargetRefs {
		if err := validateTargetRef(ref, i, ctx); err != nil {
			return err
		}
	}
	return nil
}

func validateTargetRef(ref TargetRef, idx int, ctx ValidationContext) error {
	field := fmt.Sprintf("slots.target_refs[%d]", idx)
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
		if ctx.Registry != nil {
			if matches, res := ctx.Registry.ResolveByName(ref.Value); res.Status == entity.ResolveNotFoundInAccount || len(matches) == 0 {
				return validationErr(ErrEntityNotFound, field+".value", "name target_ref does not resolve in registry")
			}
		}
		return nil
	case TargetRefUHostIDUserInput:
		if err := validateProvenance(ref, field, ctx); err != nil {
			return err
		}
		if ctx.Registry != nil {
			if _, res := ctx.Registry.ResolveByID(ref.Value); res.Status != entity.ResolveHit {
				return validationErr(ErrEntityNotFound, field+".value", "uhost_id target_ref is not in registry")
			}
		}
		return nil
	default:
		return validationErr(ErrInvalidTargetRefType, field+".type", "unsupported target_ref type")
	}
}

func validateProvenance(ref TargetRef, field string, ctx ValidationContext) error {
	if ref.Source != SourceUserText && ref.Source != SourcePriorTurn {
		return validationErr(ErrAttemptedHallucinatedEntity, field+".source", "missing or invalid entity provenance source")
	}
	sourceSpan := strings.TrimSpace(ref.SourceSpan)
	if sourceSpan == "" || utf8.RuneCountInString(sourceSpan) > 50 {
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
	for _, allowed := range AllIntents() {
		if intent == allowed {
			return true
		}
	}
	return false
}

func validRequiredTool(tool string) bool {
	switch tool {
	case "DescribeCompShareInstance",
		"GetCompShareInstanceMonitor",
		"DiagnoseBilling",
		"GetCompShareInstancePrice",
		"GetCompShareInstanceUserPrice":
		return true
	default:
		return false
	}
}

func validFilterRef(value string) bool {
	value = strings.TrimSpace(value)
	return value == "all" ||
		value == "all_running" ||
		value == "all_stopped" ||
		strings.HasPrefix(value, "gpu_type=")
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
