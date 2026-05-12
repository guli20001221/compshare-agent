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
)

type TargetRefType string

const (
	TargetRefFilter           TargetRefType = "filter"
	TargetRefName             TargetRefType = "name"
	TargetRefUHostIDUserInput TargetRefType = "uhost_id_user_input"
	TargetRefSlotPosition     TargetRefType = "slot_position"
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
		IntentUnknown,
	}
}
