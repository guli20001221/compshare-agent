package capability

import (
	"context"
	"fmt"
	"strings"

	"github.com/compshare-agent/internal/entity"
	"github.com/compshare-agent/internal/intent"
	"github.com/compshare-agent/internal/platform"
)

// Refund-estimate read capability (migrated from the legacy intent route). It
// resolves the requested instance(s), calls GetCompShareRefundPrice and renders
// a read-only estimate — it never releases the instance.

const (
	refundCapabilityLabel = string(intent.IntentRefundEstimate)
	refundAction          = "GetCompShareRefundPrice"

	// refundStaleSelectionReply — a single prior-turn target that no longer
	// resolves (deleted / not visible in this account). Parity with the legacy
	// handleRefundEstimate special case: this is a HANDLED answer, not a
	// fallback, so the agent explains rather than silently retrying.
	refundStaleSelectionReply = "未找到刚才选中的实例，可能已被删除或当前账号不可见。请重新指定实例名称或实例 ID 后再估算退费。"
)

// RefundEstimateRequest is the capability's own request contract. targets is
// required (schema required set + MissingFields), so the engine returns
// needs_input BEFORE the handler when it is empty. The legacy handler's
// FallbackInstanceID / clarify branch was therefore already unreachable on the
// read-tool path (it only served the retired direct-dispatch route) and is
// intentionally not reproduced here.
type RefundEstimateRequest struct {
	Targets []platform.TargetRef `json:"targets"`
}

func (r RefundEstimateRequest) MissingFields() []platform.MissingField {
	if len(r.Targets) == 0 {
		return []platform.MissingField{platform.Missing("targets")}
	}
	return nil
}

// RefundEstimateResponse carries the raw price payload plus the resolved
// instances used to label the rows. Terminal outcomes (unresolved / ambiguous
// target, stale prior-turn selection, upstream failure) are returned by Handle
// as a ReadResult, never here.
type RefundEstimateResponse struct {
	Raw       map[string]any
	Instances []entity.InstanceSnapshot
}

func refundReadSpec() ReadCapabilitySpec[RefundEstimateRequest, RefundEstimateResponse] {
	return ReadCapabilitySpec[RefundEstimateRequest, RefundEstimateResponse]{
		Label:       refundCapabilityLabel,
		Description: "估算指定实例当前可退金额，不执行释放。",
		Schema:      objectSchema(map[string]any{"targets": targetRefsSchema()}, []string{"targets"}),
		Handle:      refundHandle,
		Render:      refundRender,
	}
}

func refundHandle(ctx context.Context, req RefundEstimateRequest, rt ReadRuntime) (RefundEstimateResponse, ReadResult) {
	instances, ids, reason := resolveReadTargetSnapshots(req.Targets, rt.Resolver)
	if reason != nil {
		// A single prior-turn reference that no longer resolves is a stale
		// selection: answer (handled) instead of a bare fallback, matching the
		// legacy handleRefundEstimate special case.
		if len(req.Targets) == 1 && req.Targets[0].Source == platform.SourcePriorTurn {
			r := ReadHandled(refundStaleSelectionReply)
			r.ToolAction = refundAction
			return RefundEstimateResponse{}, r
		}
		return RefundEstimateResponse{}, ReadFallbackBeforeTool(*reason)
	}
	args := map[string]any{"UHostIds": ids}
	raw, err := rt.Executor.Execute(ctx, refundAction, args)
	if err != nil {
		return RefundEstimateResponse{}, ReadFailureAfterTool(refundAction, refundCapabilityLabel, err)
	}
	if raw == nil {
		raw = map[string]any{}
	}
	return RefundEstimateResponse{Raw: raw, Instances: instances}, ReadResult{}
}

func refundRender(resp RefundEstimateResponse) ReadResult {
	r := ReadHandled(renderRefundEstimateReply(resp.Raw, resp.Instances))
	r.ToolAction = refundAction
	return r
}

// --- Relocated verbatim from intent/routing_cfs_refund.go ----------------------

func renderRefundEstimateReply(raw map[string]any, instances []entity.InstanceSnapshot) string {
	rows := platform.MapSliceAt(raw, "RefundPriceSet")
	if len(rows) == 0 {
		return "未获取到退费估算结果。这个查询只做估算，不会释放实例。"
	}
	nameByID := map[string]string{}
	for _, inst := range instances {
		if inst.UHostId != "" && inst.Name != "" {
			nameByID[inst.UHostId] = inst.Name
		}
	}
	lines := []string{"退费估算结果（只读估算，不会释放实例）："}
	for _, rowAny := range rows {
		row, ok := rowAny.(map[string]any)
		if !ok {
			continue
		}
		id := stringField(row, "UHostId")
		label := id
		if name := nameByID[id]; name != "" {
			label = fmt.Sprintf("%s（%s）", name, id)
		}
		code, hasCode := numericField(row, "Code")
		if hasCode && code != 0 {
			msg := stringField(row, "Message")
			if msg == "" {
				msg = "上游暂未返回可退金额"
			}
			lines = append(lines, fmt.Sprintf("- %s：暂无法估算，%s。", label, msg))
			continue
		}
		if price, ok := numericField(row, "RefundPrice"); ok {
			lines = append(lines, fmt.Sprintf("- %s：预计可退 ¥%.2f。", label, price))
			continue
		}
		lines = append(lines, fmt.Sprintf("- %s：上游未返回可退金额。", label))
	}
	if len(lines) == 1 {
		return "未获取到有效退费估算结果。这个查询只做估算，不会释放实例。"
	}
	return strings.Join(lines, "\n")
}
