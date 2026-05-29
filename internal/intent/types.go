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
	// PR #3 (2026-05-22): pricing capability — deterministic route for
	// "X 多少钱 / X 价格 / X 包月" so commercial-critical paths don't depend
	// on LLM tool-selection variance (which produced 35s/33k-token paths
	// on baseline).
	IntentPricingQuery Intent = "pricing_query"
	// disk_info (2026-05-29): user-asks-about-attached-disks routing. The
	// upstream CompShare API exposes zero disk-list actions (verified in
	// F:/uhost-compshare-api-master/internal/api/volumn/ — only Create/
	// Delete/Resize/Attach/Detach writes). Disk facts live on the instance
	// response: pkg/api/describe_compshare_instance.go DiskSet[] + TotalDiskSpace.
	// Routing this to DescribeCompShareInstance (instead of leaking into
	// resource_info or knowledge_qa) lets the renderer foreground the
	// DiskSet view rather than the default instance summary.
	IntentDiskInfo Intent = "disk_info"
)

type TargetRefType string

const (
	TargetRefFilter           TargetRefType = "filter"
	TargetRefName             TargetRefType = "name"
	TargetRefUHostIDUserInput TargetRefType = "uhost_id_user_input"
	TargetRefSlotPosition     TargetRefType = "slot_position"

	// C15 Phase A additions (PR #89, 2026-05-21): platform-wide entity
	// types for future planner output when the user references a zone,
	// image, or GPU model directly (not their own instance). Validator
	// deliberately does NOT accept these in Phase A; the producer side
	// (planner prompt directives) and consumer side (resolver → tool
	// args) are also NOT wired. Phase B adds all three after the
	// byte-equal planner-prompt hash from C5 has been bumped
	// intentionally.
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
	// Action carries the lifecycle/configuration verb when Intent is
	// IntentOperationLifecycle. PR1 hotfix Bug 4 (2026-05-28): used by
	// engine.executeTool to deterministically pre-filter the candidate
	// instance set by State (stop/reboot → Running only, start → Stopped
	// only) before the LLM sees the list. Empty action = no filter applied
	// (conservative default for unknown verbs). See memory:
	// llm-filter-nondeterministic.
	Action LifecycleAction `json:"action,omitempty"`
}

// LifecycleAction is the verb that drives an operation_lifecycle turn. Only
// the explicit verbs below trigger state pre-filtering; an empty / unknown
// value leaves the candidate list untouched so the model still has the data
// to ask a clarifying question.
type LifecycleAction string

const (
	LifecycleActionStop       LifecycleAction = "stop"
	LifecycleActionStart      LifecycleAction = "start"
	LifecycleActionReboot     LifecycleAction = "reboot"
	LifecycleActionReinstall  LifecycleAction = "reinstall"
	LifecycleActionResize     LifecycleAction = "resize"
	LifecycleActionResetPwd   LifecycleAction = "reset_password"
	LifecycleActionRename     LifecycleAction = "rename"
	LifecycleActionCreateDisk LifecycleAction = "create_disk"
)

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
		IntentPricingQuery,
		IntentDiskInfo,
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
		IntentPricingQuery,
		IntentDiskInfo,
		IntentUnknown,
	}
}
