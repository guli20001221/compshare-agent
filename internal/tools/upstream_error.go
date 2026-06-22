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
// Hint carries optional recovery guidance (P0 阶段1B). It is NOT part of Error()
// and is surfaced to the model/user separately, so reading it can never leak the
// raw upstream tokens ("RetCode=230" / "not available" / "CompShareImageId") that
// the reply_not_contains regression gate forbids. The same Hint serves two
// consumers: the ReAct path appends it to the tool result so the model
// self-corrects (engine.go), and the direct-dispatch path uses it as the
// user-facing reply via UserMessage() (no model round runs there). The hint
// wording is therefore kept actionable for BOTH audiences.
type UpstreamAPIError struct {
	Code    int
	Message string
	Hint    string
}

func (e *UpstreamAPIError) Error() string {
	return fmt.Sprintf("API error (RetCode=%d): %s", e.Code, e.Message)
}

// UserMessage implements the intent package's userFacingError interface so the
// direct-dispatch failure path (intent.failureAfterToolForError) replies with
// the recovery hint instead of the generic "查询暂时失败" — the common real 230 /
// capacity codes are bound to direct-dispatched read routes (stock / gpu_specs /
// pricing), which never reach the ReAct hint branch. Returns "" for codes without
// a hint so the caller falls back to the generic friendly reply.
func (e *UpstreamAPIError) UserMessage() string {
	return e.Hint
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

// retCodeHint maps a known upstream RetCode to a short Chinese recovery hint, or
// "" when there is no actionable guidance. Without a hint the model only sees the
// raw "API error (RetCode=N)" and tends to blindly retry the same failing call or
// give up (the codebase's recorded create-failure root cause), and the
// direct-dispatch path falls back to the generic "查询暂时失败". Hints deliberately
// avoid the raw upstream tokens so surfacing one can never leak them into the
// final reply (eval/regression_6cat_cases.json reply_not_contains gate).
//
// The codes and their meaning are pinned to the upstream gateway source
// (uhost-compshare-api internal/errors/code.go), audited 2026-06-23:
//   - 230   CodeParamsError / CodeParamsConflictError — "Params [X] not available"
//           (a non-default zone without its Region; or an image/spec the zone
//           does not offer). The create path already prevents the zone/Region
//           case deterministically (region.go addZoneRegion + create_image_recovery),
//           so this hint mainly helps the read-tool path (e.g. a Describe/price 230).
//   - 226604 ResourceNotEnough — the real "out of GPU resources" code (emitted by
//           checkResourceCapacity → GetMachineTypeByGpuAndZone). This is the code a
//           wrong/sold-out (zone, GPU) create actually fails with, NOT 8433.
//   - 226603 GpuTypeNotSupportError — the chosen image does not support the GPU.
//   - 8433  ActionError — a GENERIC upstream service error ("retry or contact
//           support"). The pre-audit hint wrongly labelled this "out of stock";
//           capacity is 226604. Kept with a correct generic-retry wording.
//
// Unknown codes fall back to the prior raw-error behavior (no hint).
func retCodeHint(code int) string {
	switch code {
	case 230:
		// Params rejection: the zone/region/image/spec combination is not accepted.
		return "该可用区/规格/镜像组合不被接受：请更换可用区、规格或镜像后再试，不要重复同一请求。"
	case 226604:
		// Real capacity / out-of-stock code for the requested (zone, GPU).
		return "目标可用区的该卡型当前资源不足：请更换可用区或规格，或稍后再试。"
	case 226603:
		// The selected image does not support the requested GPU type.
		return "所选镜像不支持该卡型：请更换卡型或镜像后再试。"
	case 8433:
		// Generic upstream service error (NOT capacity — see 226604).
		return "服务暂时异常：请稍后重试，若持续失败请联系平台支持。"
	default:
		return ""
	}
}
