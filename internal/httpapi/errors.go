package httpapi

import (
	"errors"
	"fmt"
	"log"
	"net/http"
)

// APIError represents a structured HTTP API error.
//
// Code is the stable string identifier emitted by stream errors, HTTP error
// envelopes, and the persisted `messages.error_code` column. RetCode is the
// numeric compatibility code in the UCloud-style HTTP envelope. Status is the
// HTTP status returned to the client.
type APIError struct {
	Code    string
	RetCode int
	Status  int
	Message string
	// cause is the unclassified error this one stands in for, kept for the SERVER
	// only: it is unexported and never serialised, so it cannot reach a client
	// through the JSON envelope or a stream frame. AsAPIError sets it; the
	// response writers log it next to the request id.
	cause error
}

func (e *APIError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return e.Code
}

// Unwrap exposes the wrapped cause so errors.Is/errors.As still see through a
// converted error — dropping the text from the user-facing message must not also
// drop the typed error from code that branches on it.
func (e *APIError) Unwrap() error { return e.cause }

// Cause returns the unclassified error behind an internal APIError, or nil.
// Callers use it to LOG; putting it in a response is what this change removed.
func (e *APIError) Cause() error {
	if e == nil {
		return nil
	}
	return e.cause
}

// WithMessage returns a copy of the error with a new formatted message.
func (e *APIError) WithMessage(format string, args ...any) *APIError {
	cp := *e
	cp.Message = fmt.Sprintf(format, args...)
	return &cp
}

// RetCode values are legacy numeric compatibility values. The upstream API now
// also uses numbers in this interval, so new clients must branch on Code and no
// new numeric values should be allocated here until the platform assigns this
// service a disjoint range. Existing numbers remain unchanged for old clients.
var (
	ErrInvalidParam     = &APIError{Code: "InvalidParam", RetCode: 226612, Status: http.StatusBadRequest, Message: "参数缺失或非法"}
	ErrUnauthorized     = &APIError{Code: "Unauthorized", RetCode: 226613, Status: http.StatusUnauthorized, Message: "未登录或 token 失效"}
	ErrForbidden        = &APIError{Code: "Forbidden", RetCode: 226614, Status: http.StatusForbidden, Message: "无权访问"}
	ErrNotFound         = &APIError{Code: "NotFound", RetCode: 226615, Status: http.StatusNotFound, Message: "资源不存在"}
	ErrSessionTurnLimit = &APIError{Code: "SessionTurnLimitExceeded", RetCode: 226616, Status: http.StatusConflict, Message: "本会话轮数已达上限，请新开会话继续"}
	ErrRateLimited      = &APIError{Code: "RateLimited", RetCode: 226617, Status: http.StatusTooManyRequests, Message: "超出速率限制"}
	ErrInternal         = &APIError{Code: "InternalError", RetCode: 226618, Status: http.StatusInternalServerError, Message: "后端未预期错误"}
	ErrModelTimeout     = &APIError{Code: "ModelTimeout", RetCode: 226619, Status: http.StatusGatewayTimeout, Message: "LLM 调用超时"}
	ErrModelError       = &APIError{Code: "ModelError", RetCode: 226620, Status: http.StatusBadGateway, Message: "LLM 上游错误"}
	ErrAborted          = &APIError{Code: "Aborted", RetCode: 226621, Status: 499, Message: "用户中断"}
)

// AsAPIError converts any error into an *APIError. Returns nil if err is nil.
// If the error already is an *APIError (or wraps one), that value is returned.
// Otherwise an ErrInternal copy is returned with the original error attached as
// its cause, never as its user-facing message. Unclassified internal details
// remain available to logs through Unwrap.
func AsAPIError(err error) *APIError {
	if err == nil {
		return nil
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr
	}
	cp := *ErrInternal
	cp.cause = err
	return &cp
}

// logInternalCause records the real error behind an internal APIError, keyed by
// the request id the client was given. This is the other half of not echoing the
// text: it is the only place the operator can still read it.
func logInternalCause(context, requestID string, apiErr *APIError) {
	if cause := apiErr.Cause(); cause != nil {
		log.Printf("internal error: %s request_id=%s: %v", context, requestID, cause)
	}
}
