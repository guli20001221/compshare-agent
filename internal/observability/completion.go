package observability

// TurnCompletionTrace is the single, final classification emitted when an
// engine turn exits. It records bounded control-flow facts only: no prompt,
// reply, tool arguments, identifiers, or model-generated error text.
type TurnCompletionTrace struct {
	Class                 string              `json:"class"`
	Reason                string              `json:"reason"`
	RuntimeFinishReason   string              `json:"runtime_finish_reason,omitempty"`
	ModelCalls            int                 `json:"model_calls"`
	ModelProvider         string              `json:"model_provider,omitempty"`
	ModelIDs              []string            `json:"model_ids,omitempty"`
	ProviderFinishReasons []string            `json:"provider_finish_reasons,omitempty"`
	ModelAttempts         []ModelAttemptTrace `json:"model_attempts,omitempty"`
	ToolNames             []string            `json:"tool_names,omitempty"`
}

// ModelAttemptTrace describes one actual request to the provider. It is
// content-free; retries/fallbacks are bounded within each client call and the
// enclosing engine turn bounds the number of logical calls. FirstChunkMS is
// nil when no provider chunk was observed, distinct from a real 0ms sample.
type ModelAttemptTrace struct {
	// AttemptInCall resets for each logical Client.Chat call; it only orders the
	// provider retries/fallback requests made to obtain that one response.
	AttemptInCall int    `json:"attempt_in_call"`
	LatencyMS     int64  `json:"latency_ms"`
	Outcome       string `json:"outcome"`
	ErrorClass    string `json:"error_class,omitempty"`
	Retried       bool   `json:"retried,omitempty"`
	FinishReason  string `json:"finish_reason,omitempty"`
	FirstChunkMS  *int64 `json:"provider_first_chunk_ms,omitempty"`
}

// Confirmation attribution lives in TraceRecord.Confirmations and the outcome
// axes derived from it, not in the completion vocabulary.
const (
	CompletionClassSafetyBlock           = "safety_block"
	CompletionClassDeterministicAnswer   = "deterministic_answer"
	CompletionClassStructuredClarify     = "structured_clarification"
	CompletionClassParserFailureFallback = "parser_failure_fallback"
	CompletionClassAgent                 = "agent"
)

const (
	CompletionReasonPolicyBlock               = "policy_block"
	CompletionReasonRateLimit                 = "rate_limit"
	CompletionReasonModelOutputTruncated      = "model_output_truncated"
	CompletionReasonTokenBudget               = "token_budget"
	CompletionReasonReactRoundCeiling         = "react_round_ceiling"
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
)

func traceCompletionObserved(trace TurnCompletionTrace) bool {
	return trace.Class != "" ||
		trace.Reason != "" ||
		trace.RuntimeFinishReason != "" ||
		trace.ModelCalls != 0 ||
		trace.ModelProvider != "" ||
		len(trace.ModelIDs) > 0 ||
		len(trace.ProviderFinishReasons) > 0 ||
		len(trace.ModelAttempts) > 0 ||
		len(trace.ToolNames) > 0
}
