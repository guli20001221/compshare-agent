package intent

import (
	"context"
	"fmt"
	"strings"
)

func handleNetAcceleratorStatus(ctx context.Context, h *DemoHandler, req HandlerRequest) HandlerResult {
	const action = "CheckCompShareNetOptimizer"
	args := map[string]any{}
	raw, fb := executeRouteAction(ctx, h, req.Plan.Intent, action, args)
	if fb != nil {
		return *fb
	}
	reply := renderNetAcceleratorStatusReply(raw)
	result := HandledResult(reply)
	result.ToolAction = action
	result.ToolArgs = copyArgs(args)
	return result
}

func renderNetAcceleratorStatusReply(raw map[string]any) string {
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
		return "网络加速状态：" + strings.Join(parts, "；") + "。这是只读状态查询，不会替你开通；如需开通请到控制台网络加速入口处理。"
	}
	if optimized, ok := boolField(raw, "Optimized"); ok {
		status := "未开通"
		if optimized {
			status = "已开通"
		}
		return "网络加速" + status + "。这是只读状态查询，不会替你开通；如需开通请到控制台网络加速入口处理。"
	}
	return "未获取到网络加速状态。这是只读状态查询，不会替你开通。"
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

func stringField(m map[string]any, key string) string {
	value, ok := m[key]
	if !ok || value == nil {
		return ""
	}
	return strings.TrimSpace(fmt.Sprint(value))
}

func boolField(m map[string]any, key string) (bool, bool) {
	switch v := m[key].(type) {
	case bool:
		return v, true
	case string:
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "true", "1", "yes", "y", "on":
			return true, true
		case "false", "0", "no", "n", "off":
			return false, true
		}
	case int:
		return v != 0, true
	case int64:
		return v != 0, true
	case float64:
		return v != 0, true
	}
	return false, false
}
