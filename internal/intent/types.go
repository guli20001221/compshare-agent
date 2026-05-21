package intent

const SchemaVersion = "1.0"

type Intent string

const (
	IntentMonitorQuery              Intent = "monitor_query"
	IntentMonitorHistory            Intent = "monitor_history"
	IntentResourceInfo              Intent = "resource_info"
	IntentBillingInstance           Intent = "billing_instance"
	IntentBillingAccountUnsupported Intent = "billing_account_unsupported"
	IntentExpiryRenewal             Intent = "expiry_renewal"
	IntentDiagnosis                 Intent = "diagnosis"
	IntentVagueFailure              Intent = "vague_failure"
	IntentOperationLifecycle        Intent = "operation_lifecycle"
	IntentRecommendation            Intent = "recommendation"
	IntentKnowledgeQA               Intent = "knowledge_qa"
	IntentMixedDiagnosisKB          Intent = "mixed_diagnosis_kb"
	IntentMixedBillingKB            Intent = "mixed_billing_kb"
	IntentUnknown                   Intent = "unknown"
	// Capability Registry v1 (PR A, 2026-05-18) — declarative routing for static
	// platform queries. See internal/intent/capabilities/*.md and
	// capability_registry.go for the data-driven dispatch table.
	IntentGPUSpecsQuery      Intent = "gpu_specs_query"
	IntentStockAvailability  Intent = "stock_availability"
	IntentPlatformImageList  Intent = "platform_image_list"
	IntentCustomImageList    Intent = "custom_image_list"
	IntentCommunityImageList Intent = "community_image_list"
)

type TargetRefType string

const (
	TargetRefFilter           TargetRefType = "filter"
	TargetRefName             TargetRefType = "name"
	TargetRefUHostIDUserInput TargetRefType = "uhost_id_user_input"
	TargetRefSlotPosition     TargetRefType = "slot_position"

	// C15 Phase A additions (PR #89, 2026-05-21): platform-wide entity
	// types that the planner may emit when the user references a zone,
	// image, or GPU model directly (not their own instance). Validator
	// accepts these as well-formed; the producer side (planner prompt
	// directives) and consumer side (resolver → tool args) are NOT
	// wired in Phase A — Phase B adds those after the byte-equal
	// planner-prompt hash from C5 has been bumped intentionally.
	//
	// Producer/consumer wiring lives behind feature gates so Phase A's
	// dead-code introduction has zero runtime effect — the LLM never
	// emits these types until Phase B updates the prompt.
	TargetRefZone     TargetRefType = "zone"
	TargetRefImage    TargetRefType = "image"
	TargetRefGPUModel TargetRefType = "gpu_model"
)

type TargetSource string

const (
	SourceUserText  TargetSource = "user_text"
	SourcePriorTurn TargetSource = "prior_turn"
)

type Metric string

const (
	MetricCPU    Metric = "cpu"
	MetricMemory Metric = "memory"
	MetricGPU    Metric = "gpu"
	MetricVRAM   Metric = "vram"
)

type TimeWindowType string

const (
	TimeWindowPreset   TimeWindowType = "preset"
	TimeWindowRelative TimeWindowType = "relative"
	TimeWindowAbsolute TimeWindowType = "absolute"
)

type Plan struct {
	SchemaVersion string    `json:"schema_version"`
	Intent        Intent    `json:"intent"`
	Scope         string    `json:"scope,omitempty"`
	Slots         Slots     `json:"slots"`
	RequiredTools []string  `json:"required_tools"`
	Retrieval     Retrieval `json:"retrieval"`
	HardBlockHint bool      `json:"hard_block_hint"`
	Confidence    float64   `json:"confidence"`
	Reasoning     string    `json:"reasoning,omitempty"`
}

type Slots struct {
	TargetRefs []TargetRef `json:"target_refs,omitempty"`
	Metrics    []Metric    `json:"metrics,omitempty"`
	TimeWindow *TimeWindow `json:"time_window,omitempty"`
}

type TargetRef struct {
	Type       TargetRefType `json:"type"`
	Value      string        `json:"value"`
	Source     TargetSource  `json:"source,omitempty"`
	SourceSpan string        `json:"source_span,omitempty"`
}

type TimeWindow struct {
	Type  TimeWindowType `json:"type"`
	Value string         `json:"value"`
}

type Retrieval struct {
	Enabled bool `json:"enabled"`
}

func AllIntents() []Intent {
	return []Intent{
		IntentMonitorQuery,
		IntentMonitorHistory,
		IntentResourceInfo,
		IntentBillingInstance,
		IntentBillingAccountUnsupported,
		IntentExpiryRenewal,
		IntentDiagnosis,
		IntentVagueFailure,
		IntentOperationLifecycle,
		IntentRecommendation,
		IntentKnowledgeQA,
		IntentMixedDiagnosisKB,
		IntentMixedBillingKB,
		IntentGPUSpecsQuery,
		IntentStockAvailability,
		IntentPlatformImageList,
		IntentCustomImageList,
		IntentCommunityImageList,
		IntentUnknown,
	}
}

func RuntimeIntents() []Intent {
	return []Intent{
		IntentMonitorQuery,
		IntentMonitorHistory,
		IntentResourceInfo,
		IntentBillingInstance,
		IntentBillingAccountUnsupported,
		IntentExpiryRenewal,
		IntentDiagnosis,
		IntentVagueFailure,
		IntentOperationLifecycle,
		IntentRecommendation,
		IntentKnowledgeQA,
		IntentGPUSpecsQuery,
		IntentStockAvailability,
		IntentPlatformImageList,
		IntentCustomImageList,
		IntentCommunityImageList,
		IntentUnknown,
	}
}
