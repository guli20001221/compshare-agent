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
	ErrAttemptedHallucinatedEntity ErrorCode = "attempted_hallucinated_entity"
	ErrEntityNotFound              ErrorCode = "entity_not_found"
	ErrNameTooShort                ErrorCode = "name_too_short"
	ErrInvalidConfidence           ErrorCode = "invalid_confidence"
	ErrInvalidImageSource          ErrorCode = "invalid_image_source"
	ErrInvalidReadOnlySlot         ErrorCode = "invalid_read_only_slot"
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
	InstanceIDTokensInText(text string) []string
}

func ValidateRoute(plan IntentRoute, ctx ValidationContext) error {
	if plan.SchemaVersion != SchemaVersion {
		return validationErr(ErrInvalidSchemaVersion, "schema_version", "unsupported schema version")
	}
	if !validIntent(plan.Intent) {
		return validationErr(ErrInvalidIntent, "intent", "unknown intent")
	}
	if plan.Confidence < 0 || plan.Confidence > 1 {
		return validationErr(ErrInvalidConfidence, "confidence", "confidence must be within [0,1]")
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
	if err := validateImageSource(plan.Intent, plan.Slots.ImageSource); err != nil {
		return err
	}
	if err := validateReadOnlyRefinementSlots(plan.Intent, plan.Slots); err != nil {
		return err
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

func validateImageSource(intent Intent, source ImageSource) error {
	if source == "" {
		return nil
	}
	if intent != IntentImageList {
		return validationErr(ErrInvalidImageSource, "slots.image_source", "image_source is only valid for image_list")
	}
	if !validImageSource(source) {
		return validationErr(ErrInvalidImageSource, "slots.image_source", "unsupported image source")
	}
	return nil
}

func validImageSource(source ImageSource) bool {
	switch source {
	case ImageSourcePlatform, ImageSourceCustom, ImageSourceCommunity, ImageSourceShared:
		return true
	default:
		return false
	}
}

func validateReadOnlyRefinementSlots(intent Intent, slots Slots) error {
	if slots.ListMode != "" {
		if !validListMode(slots.ListMode) {
			return validationErr(ErrInvalidReadOnlySlot, "slots.list_mode", "unsupported list mode")
		}
		if intent != IntentImageList && intent != IntentModelRepositoryBrowse {
			return validationErr(ErrInvalidReadOnlySlot, "slots.list_mode", "list_mode is only valid for read-only list routes")
		}
	}
	if strings.TrimSpace(slots.SearchQuery) != "" && !intentAllowsSearchQuery(intent) {
		return validationErr(ErrInvalidReadOnlySlot, "slots.search_query", "search_query is only valid for read-only catalog routes")
	}
	if slots.PriceKind != "" {
		if !validPriceKind(slots.PriceKind) {
			return validationErr(ErrInvalidReadOnlySlot, "slots.price_kind", "unsupported price kind")
		}
		if intent != IntentPricingQuery {
			return validationErr(ErrInvalidReadOnlySlot, "slots.price_kind", "price_kind is only valid for pricing_query")
		}
	}
	if slots.CFSKind != "" {
		if !validCFSKind(slots.CFSKind) {
			return validationErr(ErrInvalidReadOnlySlot, "slots.cfs_kind", "unsupported CFS query kind")
		}
		if intent != IntentCFSInfo {
			return validationErr(ErrInvalidReadOnlySlot, "slots.cfs_kind", "cfs_kind is only valid for cfs_info")
		}
	}
	if slots.SizeGB < 0 {
		return validationErr(ErrInvalidReadOnlySlot, "slots.size_gb", "size_gb must be non-negative")
	}
	if slots.SizeGB > 0 && intent != IntentCFSInfo {
		return validationErr(ErrInvalidReadOnlySlot, "slots.size_gb", "size_gb is only valid for cfs_info")
	}
	if strings.TrimSpace(slots.Zone) != "" && intent != IntentCFSInfo && intent != IntentStockAvailability {
		return validationErr(ErrInvalidReadOnlySlot, "slots.zone", "zone is only valid for cfs_info or stock_availability")
	}
	if slots.ChargeType != "" {
		if !validReadOnlyChargeType(slots.ChargeType) {
			return validationErr(ErrInvalidReadOnlySlot, "slots.charge_type", "unsupported charge_type")
		}
		if intent != IntentCFSInfo {
			return validationErr(ErrInvalidReadOnlySlot, "slots.charge_type", "charge_type is only valid for cfs_info")
		}
	}
	if slots.DetailLevel != "" {
		if !validDetailLevel(slots.DetailLevel) {
			return validationErr(ErrInvalidReadOnlySlot, "slots.detail_level", "unsupported detail level")
		}
		if intent != IntentGPUSpecsQuery {
			return validationErr(ErrInvalidReadOnlySlot, "slots.detail_level", "detail_level is only valid for gpu_specs_query")
		}
	}
	return nil
}

func intentAllowsSearchQuery(intent Intent) bool {
	switch intent {
	case IntentImageList, IntentModelRepositoryBrowse, IntentPricingQuery, IntentGPUSpecsQuery, IntentStockAvailability:
		return true
	default:
		return false
	}
}

func validListMode(mode ListMode) bool {
	switch mode {
	case ListModeAll, ListModeFiltered:
		return true
	default:
		return false
	}
}

func validPriceKind(kind PriceKind) bool {
	switch kind {
	case PriceKindAccount, PriceKindCatalog:
		return true
	default:
		return false
	}
}

func validCFSKind(kind CFSKind) bool {
	switch kind {
	case CFSKindList, CFSKindCreatePrice, CFSKindUpgradePrice, CFSKindRefund:
		return true
	default:
		return false
	}
}

func validReadOnlyChargeType(value string) bool {
	switch strings.TrimSpace(value) {
	case "Month", "Year", "Day", "Dynamic", "Postpay", "Spot":
		return true
	default:
		return false
	}
}

func validDetailLevel(level DetailLevel) bool {
	switch level {
	case DetailLevelSummary, DetailLevelFull:
		return true
	default:
		return false
	}
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
