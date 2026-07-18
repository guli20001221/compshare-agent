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
	facts := BuildBillingFacts(hosts)

	total := len(facts.Instances)
	conclusion = fmt.Sprintf("您当前有 %d 个实例，费用明细如下：\n", total)
	for _, fact := range facts.Instances {
		conclusion += formatInstanceFactCost(fact) + "\n"
	}
	if facts.HasDynamic {
		if facts.HasUnknownDynamicCost {
			conclusion += "部分费用未返回，合计暂不计算。"
		} else {
			conclusion += fmt.Sprintf("按量/抢占式实例合计: ¥%.2f/时", facts.HourlyTotal)
		}
	}
	if facts.HasPrepaid {
		if facts.HasDynamic {
			conclusion += "\n"
		}
		conclusion += "包月/包日实例按预付费计费，具体金额以订单为准。"
	}

	if facts.StoppedCount > 0 && facts.StoppedRetainedTotal > 0 && !facts.HasUnknownStoppedRetained {
		costLabel := "磁盘保留费用"
		if facts.HasStoppedImageCost() {
			costLabel = "磁盘和镜像保留费用"
		}
		conclusion += fmt.Sprintf("\n\n注意：关机实例（%d 个）仍在产生%s，合计 ¥%.2f/时。", facts.StoppedCount, costLabel, facts.StoppedRetainedTotal)
	}

	switch {
	case facts.StoppedCount > 0 && facts.StoppedRetainedTotal > 0:
		releaseLabel := "磁盘保留计费"
		if facts.HasStoppedImageCost() {
			releaseLabel = "磁盘和镜像保留计费"
		}
		suggestion = fmt.Sprintf("建议释放不再使用的关机实例以停止%s，或使用定时关机功能避免空跑。", releaseLabel)
	case facts.HasDynamic && facts.RunningCount > 0:
		suggestion = "按量实例建议在不使用时关机。如长期使用，包月计费更划算，可在控制台查看包月价格对比。"
	default:
		suggestion = "如有疑问，请查看控制台费用明细页面了解详细扣费记录。"
	}

	return conclusion, suggestion
}

type BillingInstanceFact struct {
	UHostID               string
	Name                  string
	State                 string
	ChargeType            string
	IsSpot                bool
	GpuType               string
	GPU                   int
	InstancePrice         float64
	DiskPrice             float64
	ImagePrice            float64
	HasInstancePrice      bool
	HasDiskPrice          bool
	HasImagePrice         bool
	ActualComputeCharge   float64
	RetainedStoppedCharge float64
	Period                string
}

type BillingFactsSummary struct {
	Instances                 []BillingInstanceFact
	HourlyTotal               float64
	StoppedRetainedTotal      float64
	RunningCount              int
	StoppedCount              int
	HasDynamic                bool
	HasPrepaid                bool
	HasUnknownDynamicCost     bool
	HasUnknownStoppedRetained bool
}

func BuildBillingFacts(hosts []any) BillingFactsSummary {
	var summary BillingFactsSummary
	for _, h := range hosts {
		host, ok := h.(map[string]any)
		if !ok {
			continue
		}

		fact := billingInstanceFact(host)
		switch fact.State {
		case "Running":
			summary.RunningCount++
		case "Stopped":
			summary.StoppedCount++
		}

		switch {
		case fact.isHourly():
			summary.HasDynamic = true
			if fact.hasKnownHourlyTotal() {
				summary.HourlyTotal += fact.ActualComputeCharge + fact.DiskPrice + fact.ImagePrice
			} else {
				summary.HasUnknownDynamicCost = true
			}
		case fact.ChargeType == "Month" || fact.ChargeType == "Day" || fact.ChargeType == "Year":
			summary.HasPrepaid = true
		}
		if fact.State == "Stopped" {
			if fact.hasKnownStoppedRetainedCharge() {
				summary.StoppedRetainedTotal += fact.RetainedStoppedCharge
			} else {
				summary.HasUnknownStoppedRetained = true
			}
		}
		summary.Instances = append(summary.Instances, fact)
	}
	return summary
}

func billingInstanceFact(host map[string]any) BillingInstanceFact {
	id, _ := host["UHostId"].(string)
	name, _ := host["Name"].(string)
	state, _ := host["State"].(string)
	gpuType, _ := host["GpuType"].(string)
	gpu, _ := host["GPU"].(float64)
	chargeType, _ := host["ChargeType"].(string)
	// Spot instances describe as ChargeType "Postpay" (or, if billed under the
	// CHARGE_BY_SPOT enum, an empty string that maps to nothing) PLUS a separate
	// IsSpot=true flag — upstream never emits ChargeType "Spot". Key off IsSpot so
	// spot is counted/priced as hourly regardless of the ChargeType string.
	isSpot, _ := host["IsSpot"].(bool)
	instancePrice, hasInstancePrice := billingPriceField(host, "InstancePrice")
	diskPrice, hasDiskPrice := billingPriceField(host, "DiskPrice")
	imagePrice, hasImagePrice := billingPriceField(host, "CompShareImagePrice")
	return BillingInstanceFact{
		UHostID:               id,
		Name:                  name,
		State:                 state,
		ChargeType:            chargeType,
		IsSpot:                isSpot,
		GpuType:               gpuType,
		GPU:                   int(gpu),
		InstancePrice:         instancePrice,
		DiskPrice:             diskPrice,
		ImagePrice:            imagePrice,
		HasInstancePrice:      hasInstancePrice,
		HasDiskPrice:          hasDiskPrice,
		HasImagePrice:         hasImagePrice,
		ActualComputeCharge:   actualInstanceCost(state, chargeType, isSpot, instancePrice),
		RetainedStoppedCharge: retainedStoppedCharge(state, diskPrice, imagePrice),
		Period:                billingPeriod(chargeType, isSpot),
	}
}

func billingPriceField(host map[string]any, key string) (float64, bool) {
	raw, exists := host[key]
	if !exists {
		return 0, false
	}
	switch v := raw.(type) {
	case float64:
		return v, true
	case float32:
		return float64(v), true
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	case uint32:
		return float64(v), true
	case uint64:
		return float64(v), true
	default:
		return 0, false
	}
}

func (s BillingFactsSummary) HasStoppedImageCost() bool {
	for _, fact := range s.Instances {
		if fact.State == "Stopped" && fact.HasImagePrice && fact.ImagePrice > 0 {
			return true
		}
	}
	return false
}

func retainedStoppedCharge(state string, diskPrice, imagePrice float64) float64 {
	if state != "Stopped" {
		return 0
	}
	return diskPrice + imagePrice
}

// isHourly reports whether the instance bills by the hour (按量 / 抢占式). Postpay is
// the only live hourly charge type (upstream's legacy "Dynamic"/CHARGE_BY_HOUR is no
// longer used); spot is detected via IsSpot, not the ChargeType string — upstream
// renders spot as "Postpay" (or empty), never "Spot".
func (fact BillingInstanceFact) isHourly() bool {
	return fact.ChargeType == "Postpay" || fact.IsSpot
}

// actualInstanceCost returns the real billing amount for the instance portion.
// The describe API returns the configured unit price REGARDLESS of power state
// (confirmed against upstream getInstancePrice — no state check), but a stopped
// hourly instance with 关机不计费 charges ¥0 for GPU/CPU/Memory. Instances that
// keep charging while off expose PostPayShutdown=false, which the common describe
// response omits, so this stays a best-effort assumption; disk/image retention is
// surfaced separately.
func actualInstanceCost(state, chargeType string, isSpot bool, price float64) float64 {
	if state == "Stopped" && (chargeType == "Postpay" || isSpot) {
		return 0
	}
	return price
}

func formatInstanceFactCost(fact BillingInstanceFact) string {
	billing := chargeTypeLabel(fact.ChargeType, fact.IsSpot)
	actual := fact.ActualComputeCharge

	costParts := "实例费 未返回"
	if fact.HasInstancePrice {
		costParts = fmt.Sprintf("实例费 ¥%.2f", actual)
		if fact.State == "Stopped" && actual == 0 && fact.InstancePrice > 0 {
			costParts = "实例费 ¥0（已关机停计）"
		}
	}

	if fact.HasDiskPrice {
		costParts += fmt.Sprintf(" + 磁盘费 ¥%.2f", fact.DiskPrice)
	} else {
		costParts += " + 磁盘费 未返回"
	}
	if fact.HasImagePrice && fact.ImagePrice > 0 {
		costParts += fmt.Sprintf(" + 镜像费 ¥%.2f", fact.ImagePrice)
	} else if !fact.HasImagePrice {
		costParts += " + 镜像费 未返回"
	}

	return fmt.Sprintf("- %s (%s, %s×%.0f, %s, %s): %s",
		fact.UHostID, fact.Name, fact.GpuType, float64(fact.GPU), fact.State, billing, costParts)
}

func (fact BillingInstanceFact) hasKnownHourlyTotal() bool {
	return fact.HasInstancePrice && fact.HasDiskPrice && fact.HasImagePrice
}

func (fact BillingInstanceFact) hasKnownStoppedRetainedCharge() bool {
	if fact.State != "Stopped" {
		return true
	}
	return fact.HasDiskPrice && fact.HasImagePrice
}

// chargeTypeLabel returns a human-readable billing label with unit. Spot is keyed
// off IsSpot (the ChargeType string is never "Spot").
func chargeTypeLabel(chargeType string, isSpot bool) string {
	if isSpot {
		return "抢占式/时"
	}
	switch chargeType {
	case "Month":
		return "包月"
	case "Day":
		return "包日"
	case "Year":
		return "包年"
	case "Postpay":
		return "按量/时"
	default:
		return chargeType
	}
}

func billingPeriod(chargeType string, isSpot bool) string {
	if isSpot {
		return "hour"
	}
	switch chargeType {
	case "Postpay":
		return "hour"
	case "Day":
		return "day"
	case "Month":
		return "month"
	case "Year":
		return "year"
	default:
		return "unknown"
	}
}
