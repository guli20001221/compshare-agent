package tools

import (
	"errors"
	"fmt"
	"strings"
)

// UpstreamAPIError is the typed failure ExternalExecutor returns when an upstream
// CompShare response carries a non-zero RetCode.
//
// Its Error() string is BYTE-IDENTICAL to the historical flat format
// ("API error (RetCode=N): MSG"), because the saga step wrappers embed it via %v
// and that text reaches user-facing narration. Do NOT change Error()'s format.
//
// Classification, however, must NOT go through this string. Callers deciding what
// to DO about a failure read the typed fields: engine.isImageUnavailableError
// checks Code == 230 as an integer. It used to grep Error() for the "230" and
// "CompShareImageId" substrings, which matched any "230" anywhere in the sentence
// (a memory size, a byte count, part of an id) and could trigger an image swap on
// an unrelated failure.
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
	return &UpstreamAPIError{Code: code, Message: msg, Hint: retCodeGuidanceForMessage(code, msg).Hint}
}

// UpstreamAPIErrorFrom extracts an *UpstreamAPIError from an error chain.
func UpstreamAPIErrorFrom(err error) (*UpstreamAPIError, bool) {
	var apiErr *UpstreamAPIError
	if errors.As(err, &apiErr) {
		return apiErr, true
	}
	return nil, false
}

// retCodeGuidance is the one authoritative, model-safe policy for a known
// upstream RetCode. Hint is shared by direct dispatch and the Agent boundary;
// Disposition is the structured control-flow equivalent. Keeping them in one
// entry prevents a newly documented recovery hint from silently falling back to
// a different Agent disposition.
type retCodeGuidance struct {
	Hint        string
	Disposition AgentToolStatus
}

func retryLaterGuidance(hint string) retCodeGuidance {
	return retCodeGuidance{Hint: hint, Disposition: AgentToolStatusRetryLater}
}

func chooseAlternativeGuidance(hint string) retCodeGuidance {
	return retCodeGuidance{Hint: hint, Disposition: AgentToolStatusChooseAlternative}
}

func failedGuidance(hint string) retCodeGuidance {
	return retCodeGuidance{Hint: hint, Disposition: AgentToolStatusFailed}
}

// retCodeGuidanceByCode is pinned to the upstream gateway source
// (uhost-compshare-api internal/errors/code.go), audited 2026-06-26. Hints
// deliberately avoid raw upstream tokens so they can be surfaced to both the
// Agent and direct-dispatch user replies without reopening the reply_not_contains
// regression gate. Unknown RetCodes intentionally have no policy entry.
var retCodeGuidanceByCode = map[int]retCodeGuidance{
	120:    retryLaterGuidance("上游数据异常：请稍后重试，若持续失败请联系平台支持。"),
	150:    retryLaterGuidance("服务暂时不可用：请稍后重试。"),
	210:    chooseAlternativeGuidance("请求缺少必要信息：请补充实例、可用区、规格或价格所需参数后再试。"),
	220:    chooseAlternativeGuidance("请求参数超出平台允许范围：请调整数值后再试。"),
	230:    chooseAlternativeGuidance("该可用区/规格/镜像组合不被接受：请更换可用区、规格或镜像后再试，不要重复同一请求。"),
	240:    failedGuidance("当前账号没有执行该操作的权限：请确认项目、角色或资源归属。"),
	280:    chooseAlternativeGuidance("参数格式不符合要求：请检查名称、密码、时间或容量格式后再试。"),
	520:    chooseAlternativeGuidance("账号余额不足：请充值或更换计费方式后再试。"),
	8010:   chooseAlternativeGuidance("实例不是关机状态：请先关机后再执行该操作。"),
	8017:   chooseAlternativeGuidance("镜像当前不可用：请更换镜像或稍后再试。"),
	8027:   chooseAlternativeGuidance("镜像当前不可用：请更换镜像或稍后再试。"),
	8039:   chooseAlternativeGuidance("目标资源不存在或已释放：请刷新资源列表后再试。"),
	8052:   chooseAlternativeGuidance("安全组配置异常：请检查网络和安全组设置后再试。"),
	8067:   chooseAlternativeGuidance("磁盘容量参数不合法：扩盘只能增大到平台允许范围内的容量。"),
	8090:   retryLaterGuidance("价格查询失败：请稍后重试或到控制台确认费用后再操作。"),
	8095:   chooseAlternativeGuidance("资源配额不足：请释放部分资源或申请提升配额。"),
	8097:   retryLaterGuidance("账单或订单信息暂时不可用：请稍后重试或到控制台确认费用。"),
	8102:   retryLaterGuidance("资源状态更新失败：请刷新资源状态后再试。"),
	8107:   chooseAlternativeGuidance("磁盘容量参数不合法：扩盘只能增大到平台允许范围内的容量。"),
	8108:   retryLaterGuidance("账单或订单信息暂时不可用：请稍后重试或到控制台确认费用。"),
	8116:   chooseAlternativeGuidance("当前资源不支持该计费方式：请更换计费方式后再试。"),
	8117:   retryLaterGuidance("账单或订单信息暂时不可用：请稍后重试或到控制台确认费用。"),
	8226:   failedGuidance("账号认证或权限配置异常：请联系平台支持确认账号状态。"),
	8314:   chooseAlternativeGuidance("密码不符合平台规则：请使用 8-32 位并包含至少两类字符。"),
	8315:   chooseAlternativeGuidance("系统盘容量不足以使用该镜像：请先扩容系统盘或选择更小的镜像。"),
	8333:   chooseAlternativeGuidance("CPU 与内存配比不符合平台规格：请换一个推荐配置。"),
	8350:   chooseAlternativeGuidance("该共享镜像不允许再次共享：请选择其他镜像。"),
	8351:   chooseAlternativeGuidance("目标资源不存在或已释放：请刷新资源列表后再试。"),
	8357:   chooseAlternativeGuidance("当前规格资源不足：请更换可用区或规格后再试。"),
	8360:   chooseAlternativeGuidance("当前系统盘形态不支持该扩容方式：请到控制台确认磁盘状态。"),
	8366:   retryLaterGuidance("产品价格映射暂时不可用：请稍后重试或到控制台确认费用。"),
	8367:   retryLaterGuidance("资源配额查询或校验失败：请稍后重试，仍失败请联系平台支持。"),
	8372:   retryLaterGuidance("代理网络资源分配失败：请稍后重试。"),
	8374:   chooseAlternativeGuidance("CPU 平台与当前规格不匹配：请换一个平台或规格。"),
	8401:   chooseAlternativeGuidance("分页参数超出范围：请减小偏移或重新查询。"),
	8421:   retryLaterGuidance("实例元数据服务暂时不可用：请稍后重试。"),
	8433:   retryLaterGuidance("服务暂时异常：请稍后重试，若持续失败请联系平台支持。"),
	8434:   retryLaterGuidance("上游依赖服务暂时异常：请稍后重试，若持续失败请联系平台支持。"),
	8436:   chooseAlternativeGuidance("当前资源暂不允许修改：请确认资源状态和计费方式后再试。"),
	8438:   retryLaterGuidance("上游处理超时：请稍后重试，避免重复快速提交。"),
	8441:   retryLaterGuidance("操作过于频繁：请稍等一会儿再试。"),
	8442:   chooseAlternativeGuidance("该实例不支持无卡启动：请正常开机或更换支持无卡启动的实例。"),
	8443:   chooseAlternativeGuidance("已有无卡启动任务在处理中：请等待任务完成后再试。"),
	8445:   chooseAlternativeGuidance("当前账号已有无卡运行实例限制：请先恢复或关闭已有无卡实例。"),
	8498:   retryLaterGuidance("上游依赖服务暂时异常：请稍后重试，若持续失败请联系平台支持。"),
	8510:   retryLaterGuidance("上游依赖服务暂时异常：请稍后重试，若持续失败请联系平台支持。"),
	8520:   retryLaterGuidance("上游依赖服务暂时异常：请稍后重试，若持续失败请联系平台支持。"),
	8580:   retryLaterGuidance("上游依赖服务暂时异常：请稍后重试，若持续失败请联系平台支持。"),
	8903:   chooseAlternativeGuidance("实例正在执行任务：请等待当前任务完成后再操作。"),
	8905:   chooseAlternativeGuidance("实例电源状态不满足操作条件：请刷新状态后再试。"),
	8917:   chooseAlternativeGuidance("账号存在未完成订单或支付限制：请处理订单后再操作。"),
	8918:   chooseAlternativeGuidance("账号存在未完成订单或支付限制：请处理订单后再操作。"),
	8919:   chooseAlternativeGuidance("账号存在未完成订单或支付限制：请处理订单后再操作。"),
	8957:   chooseAlternativeGuidance("目标账号不存在或无权限：请确认共享或目标账号信息。"),
	8964:   chooseAlternativeGuidance("实例正在制作镜像：请等待镜像任务结束后再操作。"),
	8968:   chooseAlternativeGuidance("镜像版本名已存在：请换一个版本名称。"),
	226601: chooseAlternativeGuidance("资源已到期或不可用：请续费或选择其他资源。"),
	226602: chooseAlternativeGuidance("资源已到期或不可用：请续费或选择其他资源。"),
	226603: chooseAlternativeGuidance("所选镜像不支持该卡型：请更换卡型或镜像后再试。"),
	226604: chooseAlternativeGuidance("目标可用区的该卡型当前资源不足：请更换可用区或规格，或稍后再试。"),
	226605: retryLaterGuidance("镜像使用时长更新任务已存在：请等待任务完成后再试。"),
	226606: failedGuidance("账号实名信息缺失：请完成认证后再试。"),
	226607: chooseAlternativeGuidance("实例与容器状态不一致：请刷新状态，仍异常请联系平台支持。"),
	226608: chooseAlternativeGuidance("资源配额不足：请释放部分资源或申请提升配额。"),
	226609: retryLaterGuidance("实例操作正在处理中：请稍后刷新状态后再试。"),
	226611: chooseAlternativeGuidance("当前账号未购买对应套餐：请购买套餐后再创建相关资源。"),
	226612: chooseAlternativeGuidance("资源校验失败：请更换规格、可用区或稍后再试。"),
	226618: chooseAlternativeGuidance("共享文件存储仍被运行中的实例挂载：请先卸载或关闭相关实例。"),
	226619: retryLaterGuidance("操作过于频繁：请稍等一会儿再试。"),
	226620: retryLaterGuidance("镜像正在同步中：请等待同步完成后再操作。"),
}

func retCodeGuidanceForMessage(code int, msg string) retCodeGuidance {
	guidance := retCodeGuidanceByCode[code]
	guidance.Hint = retCodeHintForMessage(code, msg)
	return guidance
}

func retCodeHint(code int) string {
	return retCodeGuidanceForMessage(code, "").Hint
}

func retCodeHintForMessage(code int, msg string) string {
	guidance := retCodeGuidanceByCode[code]
	// Keep this pre-existing message refinement in its historical helper. The
	// RetCode policy itself remains the single table above; this only selects the
	// more specific safe hint when the gateway's code 230 identifies existing CFS.
	if code == 230 && strings.Contains(strings.ToLower(msg), "existing cfs") {
		return "该可用区已经存在 CFS 共享文件存储：请直接使用已有 CFS，或换一个支持的 Pod/容器可用区后再创建。"
	}
	return guidance.Hint
}
