package observability

import (
	"context"
	"database/sql"
	"errors"
)

// error_class.go owns the stable outcome.error_class taxonomy used by trace
// recorders. Keeping classification here prevents transport-specific labels.

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
// This is deliberately separate from httpapi.classifyChatError, which maps the
// same error to an *APIError for the HTTP response code / messages.error_code.
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
