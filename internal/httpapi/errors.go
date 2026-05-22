package httpapi

import (
	"errors"
	"fmt"
	"net/http"
)

// APIError represents a structured HTTP API error with a code, status, and message.
type APIError struct {
	Code    string
	Status  int
	Message string
}

func (e *APIError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return e.Code
}

// WithMessage returns a copy of the error with a new formatted message.
func (e *APIError) WithMessage(format string, args ...any) *APIError {
	cp := *e
	cp.Message = fmt.Sprintf(format, args...)
	return &cp
}

var (
	ErrInvalidParam            = &APIError{Code: "InvalidParam", Status: http.StatusBadRequest, Message: "参数缺失或非法"}
	ErrUnauthorized            = &APIError{Code: "Unauthorized", Status: http.StatusUnauthorized, Message: "未登录或 token 失效"}
	ErrForbidden               = &APIError{Code: "Forbidden", Status: http.StatusForbidden, Message: "无权访问"}
	ErrNotFound                = &APIError{Code: "NotFound", Status: http.StatusNotFound, Message: "资源不存在"}
	ErrSessionTurnLimit        = &APIError{Code: "SessionTurnLimitExceeded", Status: http.StatusConflict, Message: "本会话轮数已达上限，请新开会话继续"}
	ErrRateLimited             = &APIError{Code: "RateLimited", Status: http.StatusTooManyRequests, Message: "超出速率限制"}
	ErrInternal                = &APIError{Code: "InternalError", Status: http.StatusInternalServerError, Message: "后端未预期错误"}
	ErrModelTimeout            = &APIError{Code: "ModelTimeout", Status: http.StatusGatewayTimeout, Message: "LLM 调用超时"}
	ErrModelError              = &APIError{Code: "ModelError", Status: http.StatusBadGateway, Message: "LLM 上游错误"}
	ErrAborted                 = &APIError{Code: "Aborted", Status: 499, Message: "用户中断"}
)

// AsAPIError converts any error into an *APIError. Returns nil if err is nil.
// If the error already is an *APIError (or wraps one), that value is returned.
// Otherwise an ErrInternal copy carrying the original message is returned.
func AsAPIError(err error) *APIError {
	if err == nil {
		return nil
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr
	}
	return ErrInternal.WithMessage("%s", err.Error())
}
