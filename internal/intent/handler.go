package intent

import (
	"github.com/compshare-agent/internal/entity"
	"github.com/compshare-agent/internal/envelope"
)

const FriendlyToolFailureReply = "查询暂时失败，请稍后再试。"

type HandlerStatus string

const (
	HandlerStatusHandled            HandlerStatus = "handled"
	HandlerStatusNeedsInput         HandlerStatus = "needs_input"
	HandlerStatusFallbackBeforeTool HandlerStatus = "fallback_before_tool"
	HandlerStatusFailureAfterTool   HandlerStatus = "failure_after_tool"
)

// HandlerFailureClass is control-flow metadata. User-facing wording may change
// without changing whether the engine should continue into its context-aware,
// read-only agent lane.
type HandlerFailureClass string

const (
	HandlerFailureNone               HandlerFailureClass = ""
	HandlerFailureGenericRead        HandlerFailureClass = "generic_read"
	HandlerFailureActionableUpstream HandlerFailureClass = "actionable_upstream"
)

type FallbackReason string

const (
	FallbackNone             FallbackReason = ""
	FallbackMissingTarget    FallbackReason = "missing_target"
	FallbackUnresolvedTarget FallbackReason = "unresolved_target"
	FallbackAmbiguousTarget  FallbackReason = "ambiguous_target"
	FallbackTimeWindow       FallbackReason = "time_window"
	FallbackValidation       FallbackReason = "validation"
	FallbackActionNotAllowed FallbackReason = "action_not_allowed"
)

type RouteStatus string

const (
	RouteStatusNone       RouteStatus = ""
	RouteStatusDispatched RouteStatus = "dispatched"
	// RouteStatusDispatchedAgent marks a turn the agent-tier dispatch handler
	// owned (B8.3 deploy_model). Distinct from "dispatched" (fast-tier route
	// dispatch) so DeriveActualExecutionTier maps it to the agent tier rather than fast
	// — the deploy handler runs a TierAgent LLM match + the orchestrator saga.
	RouteStatusDispatchedAgent RouteStatus = "dispatched_agent"
	// RouteStatusDispatchedKnowledgeAgentLoop marks a knowledge_qa turn that the
	// knowledge_qa route sent into the shared context-aware
	// ReAct knowledge loop, instead of the terminal-RAG route
	// (dispatched_retrieval). Distinct so mainline reports tell the agent-loop
	// knowledge turn apart from BOTH the terminal-RAG route AND the deploy_model
	// agent-skill dispatch (dispatched_agent): DeriveActualExecutionPath maps it to
	// agent (the turn runs the agent loop) while DeriveActualExecutionTier maps it to
	// knowledge (it answers a knowledge question via retrieval — keeping the realized
	// knowledge-work attribution stable across the terminal→agent-loop migration).
	// Trace-only; emitted by the engine's tryPlannerDispatch, no planner prompt / SHA impact.
	RouteStatusDispatchedKnowledgeAgentLoop RouteStatus = "dispatched_knowledge_agent_loop"
	RouteStatusFallbackInvalid              RouteStatus = "fallback_invalid"
	RouteStatusFallbackLowConfidence        RouteStatus = "fallback_low_confidence"
	// RouteStatusFallbackHardBlockHint (removed PR #61, 2026-05-21):
	// planner's HardBlockHint is advisory only — no longer routes. Survives
	// in RouterTrace.HardBlockHint for analytics join with
	// EngineHardBlockTrace. Deterministic refusal comes from keyword
	// PreBlock + IntentMonitorHistory dispatcher.
	RouteStatusFallbackIneligible        RouteStatus = "fallback_ineligible"
	RouteStatusFallbackUnresolvedTarget  RouteStatus = "fallback_unresolved_target"
	RouteStatusFallbackTimeWindow        RouteStatus = "fallback_time_window"
	RouteStatusFailureAfterTool          RouteStatus = "failure_after_tool"
	RouteStatusDispatchedRetrieval       RouteStatus = "dispatched_retrieval"
	RouteStatusFallbackRetrievalMiss     RouteStatus = "fallback_retrieval_miss"
	RouteStatusFallbackRetrievalDisabled RouteStatus = "fallback_retrieval_disabled"
	RouteStatusSelectionRequired         RouteStatus = "selection_required"
)

type HandlerResult struct {
	Status HandlerStatus
	Reply  string
	// NeedsClarification marks a complete deterministic clarification rather
	// than a factual answer. The engine may let the context-aware Agent resolve
	// it when the current utterance clearly depends on a recent complete turn.
	NeedsClarification bool
	FallbackReason     FallbackReason
	RouteStatus        RouteStatus
	FailureClass       HandlerFailureClass
	ToolAction         string
	ToolArgs           map[string]any
	Envelope           *envelope.Envelope
	// RendererInputToolArgHashes records tool args consumed by deterministic
	// handler renderers before engine-level tool call ids exist. Phase 1 demo
	// populates this for monitor handler results only.
	RendererInputToolArgHashes  []string
	RendererInputEnvelopeHashes []string
	// ResourceSelectionCandidates is the ordered instance list actually surfaced
	// in a resource_info reply. Engine persists this list so a later "第 N 台 /
	// 这台" follow-up can only resolve to an item the user saw.
	ResourceSelectionCandidates []entity.InstanceSnapshot
	// ResolvedStockGpuModel is the single GPU model (API instance-type Name,
	// e.g. "4090") a stock-availability turn resolved to, or "" when the turn
	// was ambiguous / listed all models. engine.go records it into
	// SessionState.LastStockGpuModel so a later subject-eliding stock turn can
	// reuse it as the referent (RC017). Populated by handleStockAvailability only.
	ResolvedStockGpuModel string
}

