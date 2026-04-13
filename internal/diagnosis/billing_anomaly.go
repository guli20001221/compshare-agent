package diagnosis

import "fmt"

// BillingAnomalyChain returns a single-step diagnostic chain that queries instance
// billing info and produces a cost breakdown with anomaly detection.
func BillingAnomalyChain() *Chain {
	return &Chain{
		Name:        "DiagnoseBilling",
		Description: "诊断费用异常：查实例→列出收费项→解释规则",
		Steps: []Step{
			{
				Name: "查询实例计费信息",
				Tool: "DescribeCompShareInstance",
				BuildArgs: func(dCtx *Context) (map[string]any, error) {
					if id, ok := dCtx.Params["UHostId"]; ok && id != "" {
						return map[string]any{"UHostIds": []any{id}}, nil
					}
					return map[string]any{}, nil
				},
				Evaluate: func(result map[string]any, dCtx *Context) Verdict {
					uhostSet, ok := result["UHostSet"]
					if !ok {
						return Verdict{
							Action:     Conclude,
							Conclusion: "未找到任何实例。如果您仍在被扣费，可能存在未释放的资源（如云盘），请到控制台检查。",
							Suggestion: "登录控制台查看费用明细和资源列表。",
						}
					}
					hosts, ok := uhostSet.([]any)
					if !ok || len(hosts) == 0 {
						return Verdict{
							Action:     Conclude,
							Conclusion: "未找到任何实例。如果您仍在被扣费，可能存在未释放的资源（如云盘），请到控制台检查。",
							Suggestion: "登录控制台查看费用明细和资源列表。",
						}
					}
					conclusion, suggestion := buildBillingSummary(hosts)
					return Verdict{
						Action:     Conclude,
						Conclusion: conclusion,
						Suggestion: suggestion,
					}
				},
			},
		},
		Fallback: Verdict{
			Action:     Conclude,
			Conclusion: "无法获取计费信息。",
			Suggestion: "请登录控制台查看费用明细。",
		},
	}
}

func buildBillingSummary(hosts []any) (conclusion, suggestion string) {
	var (
		totalCost     float64
		runningCount  int
		stoppedCount  int
		otherCount    int
		stoppedDisk   float64
		hasDynamic    bool
		lines         []string
	)

	for _, h := range hosts {
		host, ok := h.(map[string]any)
		if !ok {
			continue
		}

		state, _ := host["State"].(string)
		instancePrice, _ := host["InstancePrice"].(float64)
		diskPrice, _ := host["DiskPrice"].(float64)

		switch state {
		case "Running":
			runningCount++
		case "Stopped":
			stoppedCount++
			stoppedDisk += diskPrice
		default:
			otherCount++
		}

		chargeType, _ := host["ChargeType"].(string)
		if chargeType == "Dynamic" {
			hasDynamic = true
		}

		totalCost += instancePrice + diskPrice
		lines = append(lines, formatInstanceCost(host))
	}

	total := len(lines)
	conclusion = fmt.Sprintf("您当前有 %d 个实例，费用明细如下：\n", total)
	for _, line := range lines {
		conclusion += line + "\n"
	}
	conclusion += fmt.Sprintf("总计: ¥%.2f/时", totalCost)

	if stoppedCount > 0 && stoppedDisk > 0 {
		conclusion += fmt.Sprintf("\n\n注意：关机实例（%d 个）仍在产生磁盘费用，合计 ¥%.2f/时。", stoppedCount, stoppedDisk)
	}

	switch {
	case stoppedCount > 0 && stoppedDisk > 0:
		suggestion = "建议释放不再使用的关机实例以停止磁盘计费，或使用定时关机功能避免空跑。"
	case hasDynamic && runningCount > 0:
		suggestion = "按量实例建议在不使用时关机。如长期使用，包月计费更划算，可通过 GetCompShareInstancePrice 对比价格。"
	default:
		suggestion = "如有疑问，请查看控制台费用明细页面了解详细扣费记录。"
	}

	return conclusion, suggestion
}

func formatInstanceCost(host map[string]any) string {
	id, _ := host["UHostId"].(string)
	name, _ := host["Name"].(string)
	gpuType, _ := host["GpuType"].(string)
	gpu, _ := host["GPU"].(float64)
	state, _ := host["State"].(string)
	instancePrice, _ := host["InstancePrice"].(float64)
	diskPrice, _ := host["DiskPrice"].(float64)

	return fmt.Sprintf("- %s (%s, %s×%.0f, %s): 实例费 ¥%.2f/时 + 磁盘费 ¥%.2f/时",
		id, name, gpuType, gpu, state, instancePrice, diskPrice)
}
