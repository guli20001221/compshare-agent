package observability

// TurnCompletionTrace is the single, final classification emitted when an
// engine turn exits. It records bounded control-flow facts only: no prompt,
// reply, tool arguments, identifiers, or model-generated error text.
type TurnCompletionTrace struct {
	Class           string   `json:"class"`
	Reason          string   `json:"reason"`
	ModelCalls      int      `json:"model_calls"`
	ContextDecision string   `json:"context_decision"`
	ReadSet         []string `json:"read_set,omitempty"`
	StateDelta      []string `json:"state_delta,omitempty"`
	// ToolScope/ToolNames describe the last actual outbound model window. A
	// multi-round ReAct turn can start from a broader window before P3 selects a
	// same-turn write lane, and a workflow/card can finish before that lane is
	// ever sent; ToolScopePhase keeps both cases explicit for rollout analysis.
	ToolScope      string `json:"tool_scope"`
	ToolScopePhase string `json:"tool_scope_phase,omitempty"`
	// ToolScopeReason is a bounded server-generated rollout/state label. It
	// never includes a user message, tenant, tool arguments, or model text.
	ToolScopeReason string   `json:"tool_scope_reason,omitempty"`
	ToolNames       []string `json:"tool_names,omitempty"`
}

const (
	CompletionClassSafetyBlock           = "safety_block"
	CompletionClassConfirmation          = "confirmation"
	CompletionClassDeterministicAnswer   = "deterministic_answer"
	CompletionClassStructuredClarify     = "structured_clarification"
	CompletionClassParserFailureFallback = "parser_failure_fallback"
	CompletionClassAgent                 = "agent"
)

const (
	CompletionReasonPolicyBlock               = "policy_block"
	CompletionReasonRateLimit                 = "rate_limit"
	CompletionReasonTokenBudget               = "token_budget"
	CompletionReasonReactRoundCeiling         = "react_round_ceiling"
	CompletionReasonConfirmationDeclined      = "confirmation_declined"
	CompletionReasonContextClarification      = "context_clarification"
	CompletionReasonSelectionRequired         = "selection_required"
	CompletionReasonMissingTimeWindow         = "missing_time_window"
	CompletionReasonDirectDispatch            = "direct_dispatch"
	CompletionReasonDirectStateMachine        = "direct_state_machine"
	CompletionReasonHandlerFailure            = "handler_failure"
	CompletionReasonRetrievalNoEvidence       = "retrieval_no_evidence"
	CompletionReasonRetrievalUnavailable      = "retrieval_unavailable"
	CompletionReasonRouteParseFailure         = "route_parse_failure"
	CompletionReasonRouteFallbackWithoutModel = "route_fallback_without_model"
	CompletionReasonAgentLoop                 = "agent_loop"
	CompletionReasonAgentDispatch             = "agent_dispatch"
	CompletionReasonModelAssistedAnswer       = "model_assisted_answer"
	CompletionReasonUnclassifiedZeroModelExit = "unclassified_zero_model_exit"
	CompletionDecisionNotInvoked              = "not_invoked"
	CompletionDecisionError                   = "error"
)

func traceCompletionObserved(trace TurnCompletionTrace) bool {
	return trace.Class != "" ||
		trace.Reason != "" ||
		trace.ModelCalls != 0 ||
		trace.ContextDecision != "" ||
		len(trace.ReadSet) > 0 ||
		len(trace.StateDelta) > 0 ||
		trace.ToolScope != "" ||
		trace.ToolScopePhase != "" ||
		trace.ToolScopeReason != "" ||
		len(trace.ToolNames) > 0
}
