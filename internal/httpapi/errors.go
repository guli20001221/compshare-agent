package httpapi

import (
	"errors"
	"fmt"
	"net/http"
)

// APIError represents a structured HTTP API error.
//
// Code is the legacy string identifier (still used by SSE error frames and by
// the persisted `messages.error_code` column). RetCode is the integer code
// emitted in the UCloud-standard JSON envelope ({"RetCode": <int>, ...}).
// Status is the HTTP status returned to the client.
type APIError struct {
	Code    string
	RetCode int
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

// RetCode integers are allocated from the platform-assigned range 226601–227000.
// 226601–226611 are taken by upstream services; new codes start at 226612.
// Keep these contiguous and update the range comment when the next code is claimed.
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
	// ErrTurnNotSaved: the model answered, but the answer and the state it produced could not
	// be committed. The reply was already streamed and cannot be unsent — but the turn did not
	// happen as far as the server is concerned, so it must NOT be reported as done. A client
	// that treats this as success carries on from a conversation the server has no record of,
	// and the NEXT turn is the one that looks like amnesia.
	//
	// "请重试" is safe here and ONLY here: nothing changed outside the database, so retrying costs
	// the user a second question and nothing else. See ErrTurnNotSavedAfterAction for the turn
	// where that sentence would be dangerous.
	ErrTurnNotSaved = &APIError{Code: "TurnNotSaved", RetCode: 226622, Status: http.StatusInternalServerError, Message: "本轮未保存，请重试"}
	// ErrTurnNotSavedAfterAction: the same failure, on a turn that EXECUTED something — created an
	// instance, started one, reset a password. The write already happened out in the world; only
	// our record of it is missing.
	//
	// Telling this user to "retry" is telling them to create a second instance. The user must be
	// pointed at the real state instead, and the client must not replay the turn. The two cases
	// carry different codes precisely so the frontend cannot handle them with one branch.
	ErrTurnNotSavedAfterAction = &APIError{
		Code:    "TurnNotSavedAfterAction",
		RetCode: 226623,
		Status:  http.StatusInternalServerError,
		Message: "本轮的操作可能已经执行，但没能保存记录。请勿重试——请先到控制台确认实例的实际状态。",
	}
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
