package intent

import "github.com/compshare-agent/internal/platform"

const SchemaVersion = "1.0"

type Intent string

const (
	IntentMonitorQuery              Intent = "monitor_query"
	IntentMonitorHistory            Intent = "monitor_history"
	IntentResourceInfo              Intent = "resource_info"
	IntentBillingInstance           Intent = "billing_instance"
	IntentBillingAccountUnsupported Intent = "billing_account_unsupported"
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
	IntentZoneCatalog           Intent = "zone_catalog"
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
	// create_instance identifies hardware-first instance creation in legacy trace
	// and evaluation records. Runtime action discovery is capability-driven.
	IntentCreateInstance Intent = "create_instance"
)

// Value-object vocabulary is owned by internal/platform (a leaf package) so a
// typed capability can consume it without importing this router package. These
// aliases keep every existing intent.TargetRef / intent.Metric / … reference
// compiling unchanged.
type TargetRefType = platform.TargetRefType

const (
	TargetRefFilter           = platform.TargetRefFilter
	TargetRefName             = platform.TargetRefName
	TargetRefUHostIDUserInput = platform.TargetRefUHostIDUserInput
	TargetRefSlotPosition     = platform.TargetRefSlotPosition
)

type TargetSource = platform.TargetSource

const (
	SourceUserText  = platform.SourceUserText
	SourcePriorTurn = platform.SourcePriorTurn
)

type Metric = platform.Metric

const (
	MetricCPU    = platform.MetricCPU
	MetricMemory = platform.MetricMemory
	MetricGPU    = platform.MetricGPU
	MetricVRAM   = platform.MetricVRAM
)

type TimeWindowType = platform.TimeWindowType

const (
	TimeWindowPreset   = platform.TimeWindowPreset
	TimeWindowRelative = platform.TimeWindowRelative
	TimeWindowAbsolute = platform.TimeWindowAbsolute
)

type ImageSource = platform.ImageSource

const (
	ImageSourcePlatform  = platform.ImageSourcePlatform
	ImageSourceCustom    = platform.ImageSourceCustom
	ImageSourceCommunity = platform.ImageSourceCommunity
	ImageSourceShared    = platform.ImageSourceShared
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
	// RequiredTools is a deprecated planner-output field. Dispatch is built from
	// the intent enum and route metadata, never from this field; router parsing
	// clears it before validation.
	RequiredTools []string `json:"required_tools"`
	// Retrieval is a deprecated planner-output field. Knowledge retrieval is
	// selected by backend route execution, never by planner JSON; router parsing
	// clears it before validation.
	Retrieval Retrieval `json:"retrieval"`
	// HardBlockHint is backend-derived for trace joining. The planner no longer
	// emits it in the response schema.
	HardBlockHint bool    `json:"hard_block_hint"`
	Confidence    float64 `json:"confidence"`
	Reasoning     string  `json:"reasoning,omitempty"`
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
	SearchQuery string      `json:"search_query,omitempty"`
	ListMode    ListMode    `json:"list_mode,omitempty"`
	PriceKind   PriceKind   `json:"price_kind,omitempty"`
	GPUCount    int         `json:"gpu_count,omitempty"`
	CFSKind     CFSKind     `json:"cfs_kind,omitempty"`
	SizeGB      int         `json:"size_gb,omitempty"`
	Zone        string      `json:"zone,omitempty"`
	ChargeType  string      `json:"charge_type,omitempty"`
	DetailLevel DetailLevel `json:"detail_level,omitempty"`
	// Action carries the lifecycle/configuration verb when Intent is
	// IntentOperationLifecycle. PR1 hotfix Bug 4 (2026-05-28): used by
	// engine.executeTool to deterministically pre-filter the candidate
	// instance set by State (stop/reboot → Running only, start → Stopped
	// only) before the LLM sees the list. Empty action = no filter applied
	// (conservative default for unknown verbs). See memory:
	// llm-filter-nondeterministic.
	Action LifecycleAction `json:"action,omitempty"`
}

type ListMode = platform.ListMode

const (
	ListModeAll      = platform.ListModeAll
	ListModeFiltered = platform.ListModeFiltered
)

type PriceKind = platform.PriceKind

const (
	PriceKindAccount = platform.PriceKindAccount
	PriceKindCatalog = platform.PriceKindCatalog
)

type CFSKind = platform.CFSKind

const (
	CFSKindList         = platform.CFSKindList
	CFSKindCreatePrice  = platform.CFSKindCreatePrice
	CFSKindUpgradePrice = platform.CFSKindUpgradePrice
	CFSKindRefund       = platform.CFSKindRefund
)

type DetailLevel = platform.DetailLevel

const (
	DetailLevelSummary = platform.DetailLevelSummary
	DetailLevelFull    = platform.DetailLevelFull
)

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

type TargetRef = platform.TargetRef

type TimeWindow = platform.TimeWindow

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
		IntentDiagnosis,
		IntentVagueFailure,
		IntentOperationLifecycle,
		IntentKnowledgeQA,
		IntentGPUSpecsQuery,
		IntentStockAvailability,
		IntentImageList,
		IntentImageTagCatalog,
		IntentZoneCatalog,
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
		IntentDiagnosis,
		IntentVagueFailure,
		IntentOperationLifecycle,
		IntentKnowledgeQA,
		IntentGPUSpecsQuery,
		IntentStockAvailability,
		IntentImageList,
		IntentImageTagCatalog,
		IntentZoneCatalog,
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
