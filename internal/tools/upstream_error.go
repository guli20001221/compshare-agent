package tools

import (
	"errors"
	"fmt"
)

// UpstreamAPIError is the typed failure ExternalExecutor returns when an upstream
// CompShare response carries a non-zero RetCode.
//
// Its Error() string is BYTE-IDENTICAL to the historical flat format
// ("API error (RetCode=N): MSG"), so every existing string-matching consumer is
// unaffected — most importantly engine.isImageUnavailableMessage, which keys off
// the "230"+"CompShareImageId" substrings to trigger zone-image auto-recovery,
// and the saga step wrappers that embed %v. Do NOT change Error()'s format.
//
// Hint carries optional recovery guidance for the model (P0 阶段1B). It is NOT
// part of Error() and the engine surfaces it into the ReAct tool result
// separately, so a model that reads the hint cannot leak the raw upstream tokens
// ("RetCode=230" / "not available" / "CompShareImageId") that the
// reply_not_contains regression gate forbids.
type UpstreamAPIError struct {
	Code    int
	Message string
	Hint    string
}

func (e *UpstreamAPIError) Error() string {
	return fmt.Sprintf("API error (RetCode=%d): %s", e.Code, e.Message)
}

// NewUpstreamAPIError builds an UpstreamAPIError, attaching a recovery hint for
// known RetCodes (empty for codes without actionable guidance).
func NewUpstreamAPIError(code int, msg string) *UpstreamAPIError {
	return &UpstreamAPIError{Code: code, Message: msg, Hint: retCodeHint(code)}
}

// UpstreamAPIErrorFrom extracts an *UpstreamAPIError from an error chain.
func UpstreamAPIErrorFrom(err error) (*UpstreamAPIError, bool) {
	var apiErr *UpstreamAPIError
	if errors.As(err, &apiErr) {
		return apiErr, true
	}
	return nil, false
}

// retCodeHint maps a known upstream RetCode to a short Chinese recovery hint for
// the model, or "" when there is no actionable guidance. Without a hint the
// model only sees the raw "API error (RetCode=N)" and tends to blindly retry the
// same failing call or give up (the codebase's recorded create-failure root
// cause). Hints deliberately avoid the raw upstream tokens so surfacing one can
// never leak them into the final reply (eval/regression_6cat_cases.json
// reply_not_contains gate).
//
// Scope is intentionally narrow (the two codes with verified meaning); unknown
// codes fall back to the prior raw-error behavior. The create-path 230 is
// already handled deterministically upstream (region.go addZoneRegion prevention
// + create_image_recovery.go auto-recovery), so this hint primarily helps the
// read-tool ReAct path (e.g. a zone-scoped Describe rejected with 230).
func retCodeHint(code int) string {
	switch code {
	case 230:
		// Params rejection: the zone/region/image/spec combination is not
		// accepted for this request (a non-default zone needs Region; or the
		// zone does not offer the requested image/spec).
		return "该请求的可用区/区域/镜像/规格组合可能不被接受：请补充 Region 参数、更换可用区，或更换镜像/规格后重试，不要原样重试同一组合。"
	case 8433:
		// Capacity / out of stock for the requested zone+spec.
		return "目标可用区与规格当前无库存：请更换可用区或规格，或稍后重试。"
	default:
		return ""
	}
}
