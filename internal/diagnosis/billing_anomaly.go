package diagnosis

import "fmt"

// BillingAnomalyChain returns a 2-step diagnostic chain that queries instance
// billing info and produces a cost breakdown with anomaly detection.
//
// Step 1: Query instance list (no UHostIds → no price, but gets IDs).
// Step 2: Re-query with explicit UHostIds to get InstancePrice/DiskPrice.
//
// This two-step design is necessary because the DescribeCompShareInstance API
// only calculates prices when UHostIds is explicitly provided (performance
// optimization on the platform side).
func BillingAnomalyChain() *Chain {
	return &Chain{
		Name:        "DiagnoseBilling",
		Description: "诊断费用异常：查实例列表→查价格详情→列出收费项→解释规则",
		Steps: []Step{
			stepBillingListInstances(),
			stepBillingQueryPrices(),
		},
		Fallback: Verdict{
			Action:     Conclude,
			Conclusion: "无法获取计费信息。",
			Suggestion: "请登录控制台查看费用明细。",
		},
	}
}

// step1: Get instance list. If user specified UHostId, skip to step2 directly.
func stepBillingListInstances() Step {
	return Step{
		Name: "查询实例列表",
		Tool: "DescribeCompShareInstance",
		BuildArgs: func(dCtx *Context) (map[string]any, error) {
			if id, ok := dCtx.Params["UHostId"]; ok && id != "" {
				// User specified a single instance — query directly with ID
				// (API returns price when UHostIds is provided)
				return map[string]any{"UHostIds": []any{id}}, nil
			}
			// Full list query (no prices, but gets IDs for step2).
			// API default Limit=20; set higher to cover all instances.
			return map[string]any{"Limit": 100}, nil
		},
		Evaluate: func(result map[string]any, dCtx *Context) Verdict {
			hosts, _ := result["UHostSet"].([]any)
			if len(hosts) == 0 {
				return Verdict{
					Action:     Conclude,
					Conclusion: "未找到任何实例。如果您仍在被扣费，可能存在未释放的资源（如云盘），请到控制台检查。",
					Suggestion: "登录控制台查看费用明细和资源列表。",
				}
			}

			// If user specified UHostId, step1 already has prices → conclude directly
			if id, ok := dCtx.Params["UHostId"]; ok && id != "" {
				conclusion, suggestion := buildBillingSummary(hosts)
				return Verdict{
					Action:     Conclude,
					Conclusion: conclusion,
					Suggestion: suggestion,
				}
			}

			// Extract UHostIds for step2 price query
			var ids []string
			for _, h := range hosts {
				host, ok := h.(map[string]any)
				if !ok {
					continue
				}
				if id, ok := host["UHostId"].(string); ok {
					ids = append(ids, id)
				}
			}
			// Store IDs in context for step2
			dCtx.Params["_billingUHostIds"] = ids
			return Verdict{Action: Continue}
		},
	}
}

// step2: Re-query with explicit UHostIds to get prices.
func stepBillingQueryPrices() Step {
	return Step{
		Name: "查询价格详情",
		Tool: "DescribeCompShareInstance",
		BuildArgs: func(dCtx *Context) (map[string]any, error) {
			ids, ok := dCtx.Params["_billingUHostIds"].([]string)
			if !ok || len(ids) == 0 {
				return nil, fmt.Errorf("no instance IDs from step 1")
			}
			// Convert []string to []any for API call
			idsAny := make([]any, len(ids))
			for i, id := range ids {
				idsAny[i] = id
			}
			return map[string]any{"UHostIds": idsAny}, nil
		},
		Evaluate: func(result map[string]any, dCtx *Context) Verdict {
			hosts, _ := result["UHostSet"].([]any)
			if len(hosts) == 0 {
				return Verdict{
					Action:     Conclude,
					Conclusion: "查询价格详情失败。",
					Suggestion: "请登录控制台查看费用明细。",
				}
			}
			conclusion, suggestion := buildBillingSummary(hosts)
			return Verdict{
				Action:     Conclude,
				Conclusion: conclusion,
				Suggestion: suggestion,
			}
		},
	}
}

func buildBillingSummary(hosts []any) (conclusion, suggestion string) {
	var (
		hourlyCost      float64 // only Dynamic/Postpay/Spot
		runningCount    int
		stoppedCount    int
		otherCount      int
		stoppedKeepCost float64 // disk + image cost for stopped instances
		hasDynamic      bool
		hasPrepaid      bool
		lines           []string
	)

	for _, h := range hosts {
		host, ok := h.(map[string]any)
		if !ok {
			continue
		}

		state, _ := host["State"].(string)
		instancePrice, _ := host["InstancePrice"].(float64)
		diskPrice, _ := host["DiskPrice"].(float64)
		imagePrice, _ := host["CompShareImagePrice"].(float64)

		switch state {
		case "Running":
			runningCount++
		case "Stopped":
			stoppedCount++
			stoppedKeepCost += diskPrice + imagePrice
		default:
			otherCount++
		}

		chargeType, _ := host["ChargeType"].(string)
		switch chargeType {
		case "Dynamic", "Postpay", "Spot":
			hasDynamic = true
			hourlyCost += actualInstanceCost(state, chargeType, instancePrice) + diskPrice + imagePrice
		case "Month", "Day":
			hasPrepaid = true
		}

		lines = append(lines, formatInstanceCost(host))
	}

	total := len(lines)
	conclusion = fmt.Sprintf("您当前有 %d 个实例，费用明细如下：\n", total)
	for _, line := range lines {
		conclusion += line + "\n"
	}
	if hasDynamic {
		conclusion += fmt.Sprintf("按量/抢占式实例合计: ¥%.2f/时", hourlyCost)
	}
	if hasPrepaid {
		if hasDynamic {
			conclusion += "\n"
		}
		conclusion += "包月/包日实例按预付费计费，具体金额以订单为准。"
	}

	if stoppedCount > 0 && stoppedKeepCost > 0 {
		costLabel := "磁盘保留费用"
		if hasStoppedImage(hosts) {
			costLabel = "磁盘和镜像保留费用"
		}
		conclusion += fmt.Sprintf("\n\n注意：关机实例（%d 个）仍在产生%s，合计 ¥%.2f/时。", stoppedCount, costLabel, stoppedKeepCost)
	}

	switch {
	case stoppedCount > 0 && stoppedKeepCost > 0:
		releaseLabel := "磁盘保留计费"
		if hasStoppedImage(hosts) {
			releaseLabel = "磁盘和镜像保留计费"
		}
		suggestion = fmt.Sprintf("建议释放不再使用的关机实例以停止%s，或使用定时关机功能避免空跑。", releaseLabel)
	case hasDynamic && runningCount > 0:
		suggestion = "按量实例建议在不使用时关机。如长期使用，包月计费更划算，可在控制台查看包月价格对比。"
	default:
		suggestion = "如有疑问，请查看控制台费用明细页面了解详细扣费记录。"
	}

	return conclusion, suggestion
}

// hasStoppedImage returns true if any stopped instance has a non-zero image cost.
func hasStoppedImage(hosts []any) bool {
	for _, h := range hosts {
		host, ok := h.(map[string]any)
		if !ok {
			continue
		}
		state, _ := host["State"].(string)
		imgPrice, _ := host["CompShareImagePrice"].(float64)
		if state == "Stopped" && imgPrice > 0 {
			return true
		}
	}
	return false
}

// actualInstanceCost returns the real billing amount for the instance portion.
// API always returns the configured unit price, but stopped Dynamic/Postpay
// instances actually charge ¥0 for GPU/CPU/Memory.
func actualInstanceCost(state, chargeType string, price float64) float64 {
	if state == "Stopped" && (chargeType == "Dynamic" || chargeType == "Postpay" || chargeType == "Spot") {
		return 0
	}
	return price
}

func formatInstanceCost(host map[string]any) string {
	id, _ := host["UHostId"].(string)
	name, _ := host["Name"].(string)
	gpuType, _ := host["GpuType"].(string)
	gpu, _ := host["GPU"].(float64)
	state, _ := host["State"].(string)
	chargeType, _ := host["ChargeType"].(string)
	instancePrice, _ := host["InstancePrice"].(float64)
	diskPrice, _ := host["DiskPrice"].(float64)
	imagePrice, _ := host["CompShareImagePrice"].(float64)

	billing := chargeTypeLabel(chargeType)
	actual := actualInstanceCost(state, chargeType, instancePrice)

	// Build the cost breakdown: instance + disk + image (if non-zero)
	costParts := fmt.Sprintf("实例费 ¥%.2f + 磁盘费 ¥%.2f", actual, diskPrice)
	if state == "Stopped" && actual == 0 && instancePrice > 0 {
		costParts = fmt.Sprintf("实例费 ¥0（已关机停计） + 磁盘费 ¥%.2f", diskPrice)
	}
	if imagePrice > 0 {
		costParts += fmt.Sprintf(" + 镜像费 ¥%.2f", imagePrice)
	}

	return fmt.Sprintf("- %s (%s, %s×%.0f, %s, %s): %s",
		id, name, gpuType, gpu, state, billing, costParts)
}

// chargeTypeLabel returns a human-readable billing label with unit.
func chargeTypeLabel(chargeType string) string {
	switch chargeType {
	case "Month":
		return "包月"
	case "Day":
		return "包日"
	case "Dynamic", "Postpay":
		return "按量/时"
	case "Spot":
		return "抢占式/时"
	default:
		return chargeType
	}
}
