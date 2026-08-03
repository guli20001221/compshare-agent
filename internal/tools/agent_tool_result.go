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

// AgentToolNextStep is a deterministic instruction for the next Agent turn.
// The Agent can still make a follow-up read after success, but it must not turn
// a retry_later/failed result into an automatic repeat of the same write.
type AgentToolNextStep string

const (
	AgentToolNextAnswerUser   AgentToolNextStep = "answer_user"
	AgentToolNextAskUser      AgentToolNextStep = "ask_user"
	AgentToolNextRetryLater   AgentToolNextStep = "retry_later"
	AgentToolNextChooseOption AgentToolNextStep = "ask_user_to_choose"
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
// five outer fields are the control plane; data is the factual payload. Keeping
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
	if !validAgentToolStatus(result.Status) || !validAgentToolNextStep(result.NextStep) || strings.TrimSpace(result.Error.Code) == "" || strings.TrimSpace(result.Meta.Action) == "" {
		return AgentToolResult{}, false
	}
	return result, true
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
	case AgentToolNextAnswerUser, AgentToolNextAskUser, AgentToolNextRetryLater, AgentToolNextChooseOption:
		return true
	default:
		return false
	}
}

// AgentToolResultFromError turns typed execution failures into the model-facing
// control plane. The RetCode mapping is deliberately based on the upstream
// API's documented/observed code families already captured by retCodeHint; raw
// RetCode messages are never copied into the Agent prompt.
func AgentToolResultFromError(action string, err error, meta AgentToolMeta) AgentToolResult {
	if err == nil {
		return AgentToolSuccess(action, nil, meta)
	}
	if apiErr, ok := UpstreamAPIErrorFrom(err); ok {
		code := "UPSTREAM_RETCODE_" + strconv.Itoa(apiErr.Code)
		message := strings.TrimSpace(apiErr.Hint)
		if message == "" {
			message = "上游服务未能完成本次请求。"
		}
		switch upstreamAgentDisposition(apiErr.Code) {
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
	case errors.Is(err, ErrActionOutcomeUncertain):
		return AgentToolFailure(action, nil, "ACTION_OUTCOME_UNCERTAIN", "操作结果无法确认，不能自动重复执行。", meta)
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

func upstreamAgentDisposition(code int) AgentToolStatus {
	switch code {
	case 120, 150, 8090, 8097, 8102, 8108, 8117, 8366, 8367, 8372, 8421, 8433, 8434, 8438, 8441, 8498, 8510, 8520, 8580, 226605, 226609, 226620:
		return AgentToolStatusRetryLater
	case 210, 220, 230, 280, 520, 8010, 8017, 8027, 8039, 8052, 8067, 8107, 8095, 8116, 8314, 8315, 8333, 8350, 8357, 8360, 8374, 8401, 8436, 8442, 8443, 8445, 8903, 8905, 8917, 8918, 8919, 8957, 8964, 8968, 226601, 226602, 226603, 226604, 226607, 226608, 226611, 226612, 226618:
		return AgentToolStatusChooseAlternative
	default:
		return AgentToolStatusFailed
	}
}
