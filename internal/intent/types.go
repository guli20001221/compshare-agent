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
	IntentKnowledgeQA               Intent = "knowledge_qa"
	IntentUnknown                   Intent = "unknown"
	// Route Registry v1 (PR A, 2026-05-18) — declarative routing for static
	// platform queries. See internal/intent/routes/*.md and
	// route_registry.go for the data-driven dispatch table.
	IntentGPUSpecsQuery         Intent = "gpu_specs_query"
	IntentStockAvailability     Intent = "stock_availability"
	IntentImageList             Intent = "image_list"
	IntentImageTagCatalog       Intent = "image_tag_catalog"
	IntentModelRepositoryBrowse Intent = "model_repository_browse"
	IntentNetAcceleratorStatus  Intent = "network_accelerator_status"
	IntentRefundEstimate        Intent = "refund_estimate"
	IntentCFSInfo               Intent = "cfs_info"
	// PR #3 (2026-05-22): pricing route — deterministic route for
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
	// deploy_model: workload-first create-family intent. The user wants to run
	// or deploy a specific model, framework, or application, and the engine
	// uses the deploy handler to match a real image before entering the existing
	// confirm-gated create flow. Keep this separate from create_instance, which
	// covers hardware-first creation.
	IntentDeployModel Intent = "deploy_model"
	// create_instance (R2b P1a, 2026-06-26): first-class create-family intent.
	// It is default-on via COMPSHARE_UNIFIED_CREATE and keeps deploy_model separate;
	// set the flag off to roll back the create_instance route during soak.
	IntentCreateInstance Intent = "create_instance"
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

type ImageSource string

const (
	ImageSourcePlatform  ImageSource = "platform"
	ImageSourceCustom    ImageSource = "custom"
	ImageSourceCommunity ImageSource = "community"
	ImageSourceShared    ImageSource = "shared"
)

type IntentRoute struct {
	SchemaVersion string `json:"schema_version"`
	Intent        Intent `json:"intent"`
	Scope         string `json:"scope,omitempty"`
	// Skills is observe-only in R0: it is code-derived after planner parsing and
	// must not participate in dispatch. Future routing-contract work may let the
	// planner propose skill candidates behind a gate.
	Skills []SelectedSkill `json:"skills,omitempty"`
	Slots  Slots           `json:"slots"`
	// RequiredTools is validation/trace-only. It does not authorize dispatch and must be derived from DispatchSpec in v2.
	//
	// ValidateRoute checks each entry against the per-intent allowlist
	// (requiredToolsForIntent), but the dispatch tool window is built solely from
	// the intent enum via the engine.visibleRegistryForIntentRoute seam
	// (intent.IntentToolSubset → tools.VisibleRegistryForSubset). The planner-emitted
	// value never reaches that construction — TestPlannerRequiredToolsDoNotAuthorizeDispatch
	// drives the same seam to enforce it. The 6b step (v2 schema) removes this from
	// planner output and keeps only the derived trace projection.
	RequiredTools []string `json:"required_tools"`
	// Retrieval is likewise validation/trace-only. ValidateRoute rejects Enabled=true
	// (stage-2A RAG is disabled), and its only engine consumer treats a stray
	// Enabled=true as route-ineligible (phase1RouteCandidateStatus, engine.go:2432) —
	// it never turns retrieval ON. Like RequiredTools it must be derived from
	// DispatchSpec in v2 rather than carried in planner output.
	Retrieval     Retrieval `json:"retrieval"`
	HardBlockHint bool      `json:"hard_block_hint"`
	Confidence    float64   `json:"confidence"`
	Reasoning     string    `json:"reasoning,omitempty"`
}

type SelectedSkill struct {
	Name       string `json:"name,omitempty"`
	Resolution string `json:"resolution"`
}

const (
	SkillResolutionDerivedFromIntent = "derived_from_intent"
	SkillResolutionAgentArm          = "agent_arm"
	SkillResolutionResolvedInReAct   = "resolved_in_react"
)

type Slots struct {
	TargetRefs  []TargetRef `json:"target_refs,omitempty"`
	Metrics     []Metric    `json:"metrics,omitempty"`
	TimeWindow  *TimeWindow `json:"time_window,omitempty"`
	ImageSource ImageSource `json:"image_source,omitempty"`
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
		IntentKnowledgeQA,
		IntentGPUSpecsQuery,
		IntentStockAvailability,
		IntentImageList,
		IntentImageTagCatalog,
		IntentModelRepositoryBrowse,
		IntentNetAcceleratorStatus,
		IntentRefundEstimate,
		IntentCFSInfo,
		IntentPricingQuery,
		IntentDiskInfo,
		IntentDeployModel,
		IntentCreateInstance,
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
		IntentKnowledgeQA,
		IntentGPUSpecsQuery,
		IntentStockAvailability,
		IntentImageList,
		IntentImageTagCatalog,
		IntentModelRepositoryBrowse,
		IntentNetAcceleratorStatus,
		IntentRefundEstimate,
		IntentCFSInfo,
		IntentPricingQuery,
		IntentDiskInfo,
		// deploy_model is a runtime intent with a dedicated engine handler. It has
		// no ReAct tool subset (the handler handles the turn and should not fall
		// through) — see tool_subset_test.go nilExpected.
		IntentDeployModel,
		IntentCreateInstance,
		IntentUnknown,
	}
}
