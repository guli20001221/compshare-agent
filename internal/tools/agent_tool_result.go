package tools

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strconv"
	"strings"

	"github.com/compshare-agent/internal/governance"
)

// AgentToolStatus is the small, closed control-flow vocabulary exposed to the
// model after a tool call. It intentionally describes what the model should do
// next, rather than mirroring every upstream or workflow-specific status.
//
// Keep this set small. A new upstream RetCode belongs in the classifier below,
// not in the model-visible status namespace.
type AgentToolStatus string

const (
	AgentToolStatusSuccess           AgentToolStatus = "success"
	AgentToolStatusNeedsInput        AgentToolStatus = "needs_input"
	AgentToolStatusRetryLater        AgentToolStatus = "retry_later"
	AgentToolStatusChooseAlternative AgentToolStatus = "choose_alternative"
	AgentToolStatusFailed            AgentToolStatus = "failed"
)

// AgentToolNextStep is a model-visible next-action label, not runtime control
// flow. Its meaning is stated once in the central prompt and pinned in tests.
// In particular, a retry_later/failed result must not become an automatic repeat
// of the same write merely because the model sees another tool round.
type AgentToolNextStep string

const (
	AgentToolNextAnswerUser       AgentToolNextStep = "answer_user"
	AgentToolNextAnswerWithLimits AgentToolNextStep = "answer_with_limits"
	AgentToolNextAskUser          AgentToolNextStep = "ask_user"
	AgentToolNextRetryLater       AgentToolNextStep = "retry_later"
	AgentToolNextChooseOption     AgentToolNextStep = "ask_user_to_choose"
	// AgentToolNextCorrectToolCall is the one next step the model owns entirely.
	// It exists because the other four all end in the user doing something, and
	// a call the model itself malformed needs nothing from the user at all.
	AgentToolNextCorrectToolCall AgentToolNextStep = "correct_tool_call"
)

// AgentToolError is always present, including successful results. Code NONE is
// used instead of a missing object so the model sees one stable JSON shape and
// can always read error.code without branching on nullable fields.
type AgentToolError struct {
	Code    string `json:"code"`
	Message string `json:"message,omitempty"`
}

// AgentToolMeta contains value-free execution facts. It must never carry raw
// user arguments, credentials, upstream error bodies, or confirmation payloads.
type AgentToolMeta struct {
	Action        string   `json:"action"`
	Attempts      int      `json:"attempts,omitempty"`
	MissingFields []string `json:"missing_fields,omitempty"`
	SourceStatus  string   `json:"source_status,omitempty"`
}

// AgentToolResult is the only normal (non-final/non-verbatim) observation that
// the central Agent receives after a tool call.
//
// data deliberately preserves the operation-specific structured result. The
// outer control fields are the control plane; data is the factual payload. Keeping
// them separate lets workflows, read capabilities, and direct API tools evolve
// without teaching the model a new error protocol for each one.
type AgentToolResult struct {
	Status    AgentToolStatus   `json:"status"`
	Data      any               `json:"data"`
	Error     AgentToolError    `json:"error"`
	Retryable bool              `json:"retryable"`
	NextStep  AgentToolNextStep `json:"next_step"`
	Meta      AgentToolMeta     `json:"meta"`
}

const agentToolNoErrorCode = "NONE"

func AgentToolSuccess(action string, data any, meta AgentToolMeta) AgentToolResult {
	return newAgentToolResult(AgentToolStatusSuccess, action, data, agentToolNoErrorCode, "", false, AgentToolNextAnswerUser, meta)
}

func AgentToolNeedsInput(action string, data any, code, message string, meta AgentToolMeta) AgentToolResult {
	return newAgentToolResult(AgentToolStatusNeedsInput, action, data, defaultAgentErrorCode(code, "MISSING_OR_INVALID_INPUT"), message, false, AgentToolNextAskUser, meta)
}

func AgentToolRetryLater(action string, data any, code, message string, meta AgentToolMeta) AgentToolResult {
	return newAgentToolResult(AgentToolStatusRetryLater, action, data, defaultAgentErrorCode(code, "UPSTREAM_TEMPORARY_FAILURE"), message, true, AgentToolNextRetryLater, meta)
}

func AgentToolChooseAlternative(action string, data any, code, message string, meta AgentToolMeta) AgentToolResult {
	return newAgentToolResult(AgentToolStatusChooseAlternative, action, data, defaultAgentErrorCode(code, "OPTION_UNAVAILABLE"), message, false, AgentToolNextChooseOption, meta)
}

func AgentToolFailure(action string, data any, code, message string, meta AgentToolMeta) AgentToolResult {
	return newAgentToolResult(AgentToolStatusFailed, action, data, defaultAgentErrorCode(code, "TOOL_EXECUTION_FAILED"), message, false, AgentToolNextAnswerUser, meta)
}

// AgentToolNoCitableEvidence represents a successful retrieval attempt that
// found no evidence suitable for confirming a platform fact. It is deliberately
// not success/answer_user: the Agent may still give stable general knowledge,
// but must limit any platform-specific answer to what it can honestly verify.
func AgentToolNoCitableEvidence(action string, data any, meta AgentToolMeta) AgentToolResult {
	return newAgentToolResult(
		AgentToolStatusFailed,
		action,
		data,
		"NO_CITABLE_EVIDENCE",
		"未检索到可用于确认平台事实的证据。",
		false,
		AgentToolNextAnswerWithLimits,
		meta,
	)
}

// AgentToolCodeInvalidArguments is the only error code that may carry
// correct_tool_call. See validAgentToolControlPlane for why the binding lives
// in the parser rather than only in the constructor below.
const AgentToolCodeInvalidArguments = "INVALID_TOOL_ARGUMENTS"

// AgentToolInvalidToolCall marks a call this binary rejected before executing
// it because the MODEL emitted arguments that are not a JSON object. The user
// supplied nothing wrong and has nothing to add, so this must never resolve to
// ask_user: the fix is for the model to re-emit the same call with valid JSON
// on the next round, which is what engine.executeToolOnce's comment has always
// said the corrective hint is for.
//
// It keeps status needs_input — the call really is missing valid input — and
// carries the whole instruction in next_step, so the closed status vocabulary
// stays at five.
func AgentToolInvalidToolCall(action string, code, message string, meta AgentToolMeta) AgentToolResult {
	return newAgentToolResult(
		AgentToolStatusNeedsInput,
		action,
		nil,
		defaultAgentErrorCode(code, AgentToolCodeInvalidArguments),
		message,
		false,
		AgentToolNextCorrectToolCall,
		meta,
	)
}

func newAgentToolResult(status AgentToolStatus, action string, data any, code, message string, retryable bool, next AgentToolNextStep, meta AgentToolMeta) AgentToolResult {
	meta.Action = strings.TrimSpace(action)
	if meta.Action == "" {
		meta.Action = "unknown"
	}
	return AgentToolResult{
		Status:    status,
		Data:      data,
		Error:     AgentToolError{Code: code, Message: strings.TrimSpace(message)},
		Retryable: retryable,
		NextStep:  next,
		Meta:      meta,
	}
}

func defaultAgentErrorCode(code, fallback string) string {
	if code = strings.TrimSpace(code); code != "" {
		return code
	}
	return fallback
}

// MarshalAgentToolResult is intentionally the sole JSON boundary for the
// agent-result contract. A non-serialisable data value becomes a safe failed
// observation rather than leaking a Go formatting artefact into the prompt.
func MarshalAgentToolResult(result AgentToolResult) string {
	payload, err := json.Marshal(result)
	if err == nil {
		return string(payload)
	}
	fallback := AgentToolFailure(result.Meta.Action, nil, "RESULT_ENCODING_FAILED", "工具结果无法安全编码，不能据此继续执行操作。", AgentToolMeta{SourceStatus: "encoding_failed"})
	payload, _ = json.Marshal(fallback)
	return string(payload)
}

// ParseAgentToolResult identifies an already-normalised observation. It is used
// at the engine boundary so a handler that deliberately emitted a typed result
// is never wrapped a second time.
func ParseAgentToolResult(raw string) (AgentToolResult, bool) {
	var result AgentToolResult
	if json.Unmarshal([]byte(raw), &result) != nil {
		return AgentToolResult{}, false
	}
	if !validAgentToolStatus(result.Status) || !validAgentToolNextStep(result.NextStep) || strings.TrimSpace(result.Error.Code) == "" || strings.TrimSpace(result.Meta.Action) == "" || !validAgentToolControlPlane(result) {
		return AgentToolResult{}, false
	}
	return result, true
}

func validAgentToolControlPlane(result AgentToolResult) bool {
	switch result.Status {
	case AgentToolStatusSuccess:
		return !result.Retryable && result.NextStep == AgentToolNextAnswerUser && result.Error.Code == agentToolNoErrorCode
	case AgentToolStatusNeedsInput:
		// Two ways a call can lack valid input, and they are answered by
		// different parties: the USER has not said something yet (ask_user), or
		// the MODEL malformed the arguments it just emitted (correct_tool_call).
		//
		// correct_tool_call is bound to one error code here, not merely produced
		// by one constructor. "Ignore the failure and call the same tool again"
		// is safe advice ONLY for arguments this binary rejected before running
		// anything; attached to an upstream failure it would be a retry loop
		// against a real side effect. A constructor-only guarantee holds until
		// the next hand-built AgentToolResult; this holds for every one of them.
		if result.NextStep == AgentToolNextCorrectToolCall {
			return !result.Retryable && result.Error.Code == AgentToolCodeInvalidArguments
		}
		// This pairing is enforced here as well as at construction because
		// agentToolObservation re-parses every result and re-wraps anything it
		// fails to recognise. A rejected pairing does not surface as an error:
		// the re-wrap reads the raw envelope's own status field, so it comes
		// back out as READ_INPUT_INCOMPLETE / ask_user with the real instruction
		// buried in data.next_step — still a question for a user who has nothing
		// to add, just not a false success.
		return !result.Retryable && result.NextStep == AgentToolNextAskUser
	case AgentToolStatusRetryLater:
		return result.Retryable && result.NextStep == AgentToolNextRetryLater
	case AgentToolStatusChooseAlternative:
		return !result.Retryable && result.NextStep == AgentToolNextChooseOption
	case AgentToolStatusFailed:
		return !result.Retryable && (result.NextStep == AgentToolNextAnswerUser || result.NextStep == AgentToolNextAnswerWithLimits)
	default:
		return false
	}
}

func validAgentToolStatus(status AgentToolStatus) bool {
	switch status {
	case AgentToolStatusSuccess, AgentToolStatusNeedsInput, AgentToolStatusRetryLater, AgentToolStatusChooseAlternative, AgentToolStatusFailed:
		return true
	default:
		return false
	}
}

func validAgentToolNextStep(step AgentToolNextStep) bool {
	switch step {
	case AgentToolNextAnswerUser, AgentToolNextAnswerWithLimits, AgentToolNextAskUser,
		AgentToolNextRetryLater, AgentToolNextChooseOption, AgentToolNextCorrectToolCall:
		return true
	default:
		return false
	}
}

// AgentToolResultFromError turns typed execution failures into the model-facing
// control plane. The RetCode mapping is deliberately based on the upstream
// API's documented/observed code families already captured by retCodeHint. Raw
// RetCode messages are never copied into the Agent prompt: for unknown codes the
// stable error code is preserved, while operators diagnose the original error in
// protected traces rather than turning an upstream body into model context.
func AgentToolResultFromError(action string, err error, meta AgentToolMeta) AgentToolResult {
	if err == nil {
		return AgentToolSuccess(action, nil, meta)
	}
	if apiErr, ok := UpstreamAPIErrorFrom(err); ok {
		code := "UPSTREAM_RETCODE_" + strconv.Itoa(apiErr.Code)
		guidance := retCodeGuidanceForMessage(apiErr.Code, apiErr.Message)
		message := strings.TrimSpace(guidance.Hint)
		if message == "" {
			message = "上游服务未能完成本次请求。"
		}
		switch guidance.Disposition {
		case AgentToolStatusRetryLater:
			return AgentToolRetryLater(action, nil, code, message, meta)
		case AgentToolStatusChooseAlternative:
			return AgentToolChooseAlternative(action, nil, code, message, meta)
		default:
			return AgentToolFailure(action, nil, code, message, meta)
		}
	}

	switch {
	case errors.Is(err, governance.ErrRateLimited):
		return AgentToolRetryLater(action, nil, "RATE_LIMITED", "请求过于频繁，请稍后再试。", meta)
	case errors.Is(err, context.DeadlineExceeded):
		return AgentToolRetryLater(action, nil, "UPSTREAM_TIMEOUT", "上游处理超时，请稍后再试。", meta)
	case errors.Is(err, ErrUserDeclined):
		return AgentToolFailure(action, nil, "CONFIRMATION_NOT_ACCEPTED", "用户尚未确认，本次操作没有执行。", meta)
	case errors.Is(err, ErrToolCapExceeded):
		return AgentToolFailure(action, nil, "TOOL_LIMIT_REACHED", "本次请求超出工具允许范围。", meta)
	case errors.Is(err, ErrHistoryWindowExceeded):
		return AgentToolNeedsInput(action, nil, "HISTORY_WINDOW_INVALID", "查询时间范围不符合要求，请缩小范围后再试。", meta)
	case errors.Is(err, ErrMutatingActionDisabled):
		return AgentToolFailure(action, nil, "MUTATION_DISABLED", "当前入口不能执行写操作。", meta)
	case errors.Is(err, ErrDestructiveAction):
		return AgentToolFailure(action, nil, "DESTRUCTIVE_ACTION_REFUSED", "该破坏性操作不能由 Agent 执行。", meta)
	case errors.Is(err, ErrCFSZoneUnresolved):
		return AgentToolRetryLater(action, nil, "RESOURCE_LOCATION_UNRESOLVED", "暂时无法确认资源所在可用区，请稍后重试。", meta)
	}

	var netErr net.Error
	if errors.As(err, &netErr) || errors.Is(err, io.EOF) {
		return AgentToolRetryLater(action, nil, "UPSTREAM_NETWORK_ERROR", "网络或上游连接暂时异常，请稍后重试。", meta)
	}
	return AgentToolFailure(action, nil, "TOOL_EXECUTION_FAILED", "工具未能完成本次请求。", meta)
}
