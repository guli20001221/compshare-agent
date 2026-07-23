package intent

// HandlerFailureClass is control-flow metadata. User-facing wording may change
// without changing whether the engine should continue into its context-aware,
// read-only agent lane.
type HandlerFailureClass string

const (
	HandlerFailureNone               HandlerFailureClass = ""
	HandlerFailureGenericRead        HandlerFailureClass = "generic_read"
	HandlerFailureActionableUpstream HandlerFailureClass = "actionable_upstream"
)

type RouteStatus string

const (
	RouteStatusNone       RouteStatus = ""
	RouteStatusDispatched RouteStatus = "dispatched"
	// RouteStatusDispatchedAgent is retained only for legacy trace compatibility.
	// Runtime work now runs through the central Agent and typed workflows.
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
	// Trace-only; emitted by the engine when it routes a knowledge_qa turn into the
	// agent loop (the pre-P6 tryPlannerDispatch that named this is gone), no prompt / SHA impact.
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
