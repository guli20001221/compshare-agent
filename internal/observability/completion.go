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
	ToolScope       string   `json:"tool_scope"`
	ToolNames       []string `json:"tool_names,omitempty"`
}

// The confirmation class and its reasons were removed with the premature
// completion lock that produced them: confirmation attribution now lives in
// TraceRecord.Confirmations and the outcome axes derived from it. Nothing
// writes a confirmation completion, so naming one here would be vocabulary
// with no producer.
const (
	CompletionClassSafetyBlock           = "safety_block"
	CompletionClassDeterministicAnswer   = "deterministic_answer"
	CompletionClassStructuredClarify     = "structured_clarification"
	CompletionClassParserFailureFallback = "parser_failure_fallback"
	CompletionClassAgent                 = "agent"
)

const (
	CompletionReasonPolicyBlock                  = "policy_block"
	CompletionReasonRateLimit                    = "rate_limit"
	CompletionReasonTokenBudget                  = "token_budget"
	CompletionReasonReactRoundCeiling            = "react_round_ceiling"
	CompletionReasonContextClarification         = "context_clarification"
	CompletionReasonSelectionRequired            = "selection_required"
	CompletionReasonMissingTimeWindow            = "missing_time_window"
	CompletionReasonDirectDispatch               = "direct_dispatch"
	CompletionReasonDirectStateMachine           = "direct_state_machine"
	CompletionReasonHandlerFailure               = "handler_failure"
	CompletionReasonRetrievalNoEvidence          = "retrieval_no_evidence"
	CompletionReasonRetrievalUnavailable         = "retrieval_unavailable"
	CompletionReasonRouteParseFailure            = "route_parse_failure"
	CompletionReasonRouteFallbackWithoutModel    = "route_fallback_without_model"
	CompletionReasonAgentLoop                    = "agent_loop"
	CompletionReasonAgentDispatch                = "agent_dispatch"
	CompletionReasonModelAssistedAnswer          = "model_assisted_answer"
	CompletionReasonUnclassifiedZeroModelExit    = "unclassified_zero_model_exit"
	CompletionDecisionNotInvoked                 = "not_invoked"
	CompletionDecisionError                      = "error"
)

func traceCompletionObserved(trace TurnCompletionTrace) bool {
	return trace.Class != "" ||
		trace.Reason != "" ||
		trace.ModelCalls != 0 ||
		trace.ContextDecision != "" ||
		len(trace.ReadSet) > 0 ||
		len(trace.StateDelta) > 0 ||
		trace.ToolScope != "" ||
		len(trace.ToolNames) > 0
}
