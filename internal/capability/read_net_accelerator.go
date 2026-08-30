package capability

import (
	"context"
	"fmt"
	"strings"

	"github.com/compshare-agent/internal/intent"
	"github.com/compshare-agent/internal/platform"
)

// Network-accelerator status is account-scoped; the upstream action takes no
// instance argument.

const (
	netAcceleratorCapabilityLabel = string(intent.IntentNetAcceleratorStatus)
	netAcceleratorAction          = "CheckCompShareNetOptimizer"
)

// NetworkAcceleratorStatusRequest is the capability's own request contract.
type NetworkAcceleratorStatusRequest struct{}

// MissingFields: none — a network-accelerator status query is account-scoped.
func (NetworkAcceleratorStatusRequest) MissingFields() []platform.MissingField { return nil }

// NetworkAcceleratorStatusResponse carries the rendered status reply.
type NetworkAcceleratorStatusResponse struct {
	Reply string
}

func netAcceleratorReadSpec() ReadCapabilitySpec[NetworkAcceleratorStatusRequest, NetworkAcceleratorStatusResponse] {
	return ReadCapabilitySpec[NetworkAcceleratorStatusRequest, NetworkAcceleratorStatusResponse]{
		Label:       netAcceleratorCapabilityLabel,
		Description: "查各地域的代码仓库等平台网络加速状态；不查实例 EIP 共享带宽。",
		Params:      objectParam(map[string]schemaNode{}),
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
			location := strings.TrimSpace(strings.Join([]string{row.region, row.zone}, " "))
			if location == "" {
				parts = append(parts, status)
			} else {
				parts = append(parts, fmt.Sprintf("%s %s", location, status))
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

// netAcceleratorBoundaryNote offers activation only when at least one queried
// region is disabled.
func netAcceleratorBoundaryNote(anyDisabled bool) string {
	if anyDisabled {
		return "这是只读状态查询，不会直接修改配置；如需开通，我会走确认流程。"
	}
	return "这是只读状态查询，不会直接修改配置。"
}

type netAcceleratorRow struct {
	region    string
	zone      string
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
			zone:      stringField(entry, "Zone"),
			optimized: optimized,
		})
	}
	return rows
}
