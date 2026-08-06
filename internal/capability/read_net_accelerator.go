package capability

import (
	"context"
	"fmt"
	"strings"

	"github.com/compshare-agent/internal/intent"
	"github.com/compshare-agent/internal/platform"
)

// Network-accelerator status read capability (migrated from the legacy intent
// route). The legacy handler ignored the resolved targets entirely —
// CheckCompShareNetOptimizer is account-scoped and takes no instance argument —
// so this capability decodes Targets for schema parity but the handler makes
// the same bare call.

const (
	netAcceleratorCapabilityLabel = string(intent.IntentNetAcceleratorStatus)
	netAcceleratorAction          = "CheckCompShareNetOptimizer"
)

// NetworkAcceleratorStatusRequest is the capability's own request contract.
type NetworkAcceleratorStatusRequest struct {
	Targets []platform.TargetRef `json:"targets,omitempty"`
}

// MissingFields: none — a network-accelerator status query is account-scoped.
func (NetworkAcceleratorStatusRequest) MissingFields() []platform.MissingField { return nil }

// NetworkAcceleratorStatusResponse carries the rendered status reply. There is
// no evidence envelope for this capability (parity with the legacy route); the
// reply is produced in Handle and passed through Render so the Handle/Render
// split matches the kernel contract.
type NetworkAcceleratorStatusResponse struct {
	Reply string
}

func netAcceleratorReadSpec() ReadCapabilitySpec[NetworkAcceleratorStatusRequest, NetworkAcceleratorStatusResponse] {
	return ReadCapabilitySpec[NetworkAcceleratorStatusRequest, NetworkAcceleratorStatusResponse]{
		Label:       netAcceleratorCapabilityLabel,
		Description: "查询当前账号各地域或可用区的网络加速状态。只读，不开启或关闭网络加速。",
		Params:      objectParam(map[string]schemaNode{"targets": targetRefsParam()}),
		Handle:      netAcceleratorHandle,
		Render:      netAcceleratorRender,
	}
}

func netAcceleratorHandle(ctx context.Context, _ NetworkAcceleratorStatusRequest, rt ReadRuntime) (NetworkAcceleratorStatusResponse, ReadResult) {
	raw, err := rt.Executor.Execute(ctx, netAcceleratorAction, map[string]any{})
	if err != nil {
		return NetworkAcceleratorStatusResponse{}, ReadFailureAfterTool(netAcceleratorAction, netAcceleratorCapabilityLabel, err)
	}
	if raw == nil {
		raw = map[string]any{}
	}
	reply, empty := renderNetAcceleratorStatusReply(raw)
	if empty {
		return NetworkAcceleratorStatusResponse{}, ReadEmpty(reply)
	}
	return NetworkAcceleratorStatusResponse{Reply: reply}, ReadResult{}
}

func netAcceleratorRender(resp NetworkAcceleratorStatusResponse) ReadResult {
	r := ReadHandled(resp.Reply)
	r.ToolAction = netAcceleratorAction
	return r
}

// --- Relocated verbatim from intent/routing_net_accelerator.go -----------------

// renderNetAcceleratorStatusReply returns the status reply and whether the query
// yielded no status at all (no rows and no Optimized field) — an empty status is
// a structured Empty read, not a Handled answer.
func renderNetAcceleratorStatusReply(raw map[string]any) (string, bool) {
	rows := netAcceleratorRows(raw)
	if len(rows) > 0 {
		parts := make([]string, 0, len(rows))
		for _, row := range rows {
			status := "未开通"
			if row.optimized {
				status = "已开通"
			}
			if row.region == "" {
				parts = append(parts, status)
			} else {
				parts = append(parts, fmt.Sprintf("%s %s", row.region, status))
			}
		}
		anyDisabled := false
		for _, row := range rows {
			if !row.optimized {
				anyDisabled = true
			}
		}
		return "网络加速状态：" + strings.Join(parts, "；") + "。" + netAcceleratorBoundaryNote(anyDisabled), false
	}
	if optimized, ok := boolField(raw, "Optimized"); ok {
		status := "未开通"
		if optimized {
			status = "已开通"
		}
		return "网络加速" + status + "。" + netAcceleratorBoundaryNote(!optimized), false
	}
	return "未获取到网络加速状态。这是只读状态查询，不会直接修改配置。", true
}

// netAcceleratorBoundaryNote states the read-only boundary, and offers the
// 开通 path only when something is actually off.
//
// It used to be one fixed string, so a reply read 「网络加速已开通。……如需开通，我会走
// 确认流程。」 — offering to turn on what the same sentence had just reported as on.
// The user cannot tell whether the status or the offer is the mistake, so the
// whole answer becomes untrustworthy. With several regions the offer is still
// correct as long as ONE of them is off, which is why this takes a flag rather
// than being deleted.
func netAcceleratorBoundaryNote(anyDisabled bool) string {
	if anyDisabled {
		return "这是只读状态查询，不会直接修改配置；如需开通，我会走确认流程。"
	}
	return "这是只读状态查询，不会直接修改配置。"
}

type netAcceleratorRow struct {
	region    string
	optimized bool
}

func netAcceleratorRows(raw map[string]any) []netAcceleratorRow {
	values, ok := raw["Info"].([]any)
	if !ok {
		return nil
	}
	rows := make([]netAcceleratorRow, 0, len(values))
	for _, value := range values {
		entry, ok := value.(map[string]any)
		if !ok {
			continue
		}
		optimized, ok := boolField(entry, "Optimized")
		if !ok {
			continue
		}
		rows = append(rows, netAcceleratorRow{
			region:    stringField(entry, "Region"),
			optimized: optimized,
		})
	}
	return rows
}
