package observability

import (
	"context"
	"database/sql"
	"errors"
)

// error_class.go is the single, shared chat-error classifier for the trace's
// outcome.error_class axis. Before this, the CLI recorder discarded chatErr
// entirely (cmd/trace.go: `_ = chatErr`) and the HTTP recorder only synthesized a
// coarse "chat_error" hard-block — so the two paths produced no error_class / a
// different one (critique must-fix #1). Routing both recorders through this one
// function makes the axis consistent.

// ErrorClass* are the coarse trace labels for outcome.error_class. They classify
// WHY a turn errored, at a granularity useful for GROUP BY in the dashboard — not
// the full upstream error. Keep the set small and stable.
const (
	ErrorClassContextCanceled = "context_canceled"
	ErrorClassTimeout         = "timeout"
	ErrorClassNotFound        = "not_found"
	ErrorClassModelError      = "model_error"
)

// ClassifyErrorClass maps a chat error to a coarse, stable trace label. Returns
// "" for a nil error (a non-error turn carries no error_class).
//
// This is deliberately SEPARATE from httpapi.classifyChatError, which maps the
// same error to an *APIError for the HTTP response code / messages.error_code —
// an HTTP-protocol concern carrying httpapi types. The CLI path has no HTTP
// response, so the only divergence that ever mattered for observability is the
// trace label, and that is what this unifies across both recorders.
func ClassifyErrorClass(err error) string {
	if err == nil {
		return ""
	}
	switch {
	case errors.Is(err, context.Canceled):
		return ErrorClassContextCanceled
	case errors.Is(err, context.DeadlineExceeded):
		return ErrorClassTimeout
	case errors.Is(err, sql.ErrNoRows):
		return ErrorClassNotFound
	default:
		return ErrorClassModelError
	}
}
