package diagnosis

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/compshare-agent/internal/entity"
	"github.com/compshare-agent/internal/tools"
)

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
		Name: "DiagnoseBilling",
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
		Execute: executeBillingInstancePages,
		Evaluate: func(result map[string]any, dCtx *Context) Verdict {
			hosts, _ := result["UHostSet"].([]any)
			if total, ok := billingIntegerField(result, "TotalCount"); ok {
				dCtx.Params["_billingTotalCount"] = total
			}
			if len(hosts) == 0 {
				if id, ok := dCtx.Params["UHostId"].(string); ok && strings.TrimSpace(id) != "" {
					return Verdict{
						Action:     Conclude,
						Conclusion: "本次未找到指定实例，未取得其当前报价；不能据此判断账号下没有其他实例或资源，也不能判断历史实际扣款。",
						Suggestion: "请核对实例 ID；历史实际扣款请查看控制台账单流水。",
					}
				}
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
		Execute: executeBillingPriceBatches,
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
			if total, ok := dCtx.Params["_billingTotalCount"].(int); ok && total > len(hosts) {
				conclusion += fmt.Sprintf("\n本次只取得 %d/%d 个实例的价格，不能据此计算全账号合计。", len(hosts), total)
			}
			return Verdict{
				Action:     Conclude,
				Conclusion: conclusion,
				Suggestion: suggestion,
			}
		},
	}
}

func executeBillingInstancePages(ctx context.Context, executor tools.ToolExecutor, args map[string]any) (map[string]any, error) {
	if _, targeted := args["UHostIds"]; targeted {
		return executor.Execute(ctx, "DescribeCompShareInstance", args)
	}
	const pageSize = 100
	merged := map[string]any{"UHostSet": []any{}}
	all := make([]any, 0, pageSize)
	for offset := 0; ; offset += pageSize {
		pageArgs := cloneBillingArgs(args)
		pageArgs["Offset"] = offset
		pageArgs["Limit"] = pageSize
		page, err := executor.Execute(ctx, "DescribeCompShareInstance", pageArgs)
		if err != nil {
			return nil, err
		}
		if offset == 0 {
			for key, value := range page {
				merged[key] = value
			}
		}
		rows, _ := page["UHostSet"].([]any)
		all = append(all, rows...)
		total, hasTotal := billingIntegerField(page, "TotalCount")
		if len(rows) < pageSize || (hasTotal && len(all) >= total) {
			break
		}
	}
	merged["UHostSet"] = all
	merged["TotalCount"] = len(all)
	return merged, nil
}

func executeBillingPriceBatches(ctx context.Context, executor tools.ToolExecutor, args map[string]any) (map[string]any, error) {
	ids, ok := args["UHostIds"].([]any)
	if !ok || len(ids) <= 100 {
		return executor.Execute(ctx, "DescribeCompShareInstance", args)
	}
	merged := map[string]any{"UHostSet": []any{}}
	all := make([]any, 0, len(ids))
	for start := 0; start < len(ids); start += 100 {
		end := start + 100
		if end > len(ids) {
			end = len(ids)
		}
		batchArgs := cloneBillingArgs(args)
		batchArgs["UHostIds"] = append([]any(nil), ids[start:end]...)
		page, err := executor.Execute(ctx, "DescribeCompShareInstance", batchArgs)
		if err != nil {
			return nil, err
		}
		if start == 0 {
			for key, value := range page {
				merged[key] = value
			}
		}
		rows, _ := page["UHostSet"].([]any)
		all = append(all, rows...)
	}
	merged["UHostSet"] = all
	merged["TotalCount"] = len(all)
	return merged, nil
}

func cloneBillingArgs(args map[string]any) map[string]any {
	out := make(map[string]any, len(args)+2)
	for key, value := range args {
		out[key] = value
	}
	return out
}

func buildBillingSummary(hosts []any) (conclusion, suggestion string) {
	facts := BuildBillingFacts(hosts)

	total := len(facts.Instances)
	conclusion = fmt.Sprintf("查到 %d 个实例。以下是上游返回的当前配置净报价，不是历史账单或已扣金额：\n", total)
	for _, fact := range facts.Instances {
		conclusion += formatInstanceFactCost(fact) + "\n"
	}
	periods := make([]string, 0, len(facts.CurrentTotals))
	for period := range facts.CurrentTotals {
		periods = append(periods, period)
	}
	sort.Strings(periods)
	for _, period := range periods {
		conclusion += fmt.Sprintf("\n可确定的当前费用合计：¥%.2f%s", facts.CurrentTotals[period], billingUnitSuffix(period))
	}
	if facts.HasUnknownCurrentCost {
		conclusion += "\n部分当前费用缺少结构化价格或关机保留信息，未纳入合计。"
	}
	conclusion += "\n磁盘价格是上游按该资源、区域和计费方式算出的净价；系统盘免费额度如适用已在上游扣除，但接口不返回额度数值，不能从价格反推。数据盘和容器 CVolume 也没有免费额度字段。"

	switch {
	case facts.StoppedCount > 0 && facts.StoppedRetainedTotal > 0:
		suggestion = "关机后仍计费的磁盘已按上游返回单独标出；不再使用时需释放对应资源，关机本身不会释放磁盘。"
	case facts.HasDynamic && facts.RunningCount > 0:
		suggestion = "这里反映当前配置的单位报价；如要核对某一时间段实际扣费，仍需查询账单流水。"
	default:
		suggestion = "如要核对实际扣款，请以控制台账单流水为准。"
	}

	return conclusion, suggestion
}

type BillingInstanceFact struct {
	UHostID              string
	Name                 string
	State                string
	ChargeType           string
	IsSpot               bool
	GpuType              string
	GPU                  int
	InstancePrice        float64
	DiskPrice            float64
	ImagePrice           float64
	HasInstancePrice     bool
	HasDiskPrice         bool
	HasImagePrice        bool
	ActualComputeCharge  float64
	Period               string
	Region               string
	Zone                 string
	Components           []BillingCostComponent
	HasDiskBreakdown     bool
	HasPowerOffBreakdown bool
}

type BillingCostComponent struct {
	Kind                string
	ChargeType          string
	Period              string
	Price               float64
	Known               bool
	IsBoot              bool
	RetainedWhenStopped bool
}

type BillingFactsSummary struct {
	Instances                 []BillingInstanceFact
	HourlyTotal               float64
	StoppedRetainedTotal      float64
	RunningCount              int
	StoppedCount              int
	HasDynamic                bool
	HasPrepaid                bool
	HasUnknownStoppedRetained bool
	CurrentTotals             map[string]float64
	HasUnknownCurrentCost     bool
}

func BuildBillingFacts(hosts []any) BillingFactsSummary {
	summary := BillingFactsSummary{CurrentTotals: map[string]float64{}}
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
		case fact.ChargeType == "Month" || fact.ChargeType == "Day" || fact.ChargeType == "Year":
			summary.HasPrepaid = true
		}
		accumulateCurrentCosts(&summary, fact)
		summary.Instances = append(summary.Instances, fact)
	}
	return summary
}

func billingInstanceFact(host map[string]any) BillingInstanceFact {
	id, _ := host["UHostId"].(string)
	name, _ := host["Name"].(string)
	state, _ := host["State"].(string)
	region, _ := host["Region"].(string)
	zone, _ := host["Zone"].(string)
	gpuType, _ := host["GpuType"].(string)
	gpu, _ := host["GPU"].(float64)
	chargeType, _ := host["ChargeType"].(string)
	// Spot instances describe as ChargeType "Postpay" (or, if billed under the
	// CHARGE_BY_SPOT enum, an empty string) plus IsSpot=true. Share the flag reader
	// with the resource projection so both paths classify the same row alike.
	isSpot := entity.InstanceIsSpot(host)
	instancePrice, hasInstancePrice := billingPriceField(host, "InstancePrice")
	diskPrice, hasDiskPrice := billingPriceField(host, "DiskPrice")
	imagePrice, hasImagePrice := billingPriceField(host, "CompShareImagePrice")
	fact := BillingInstanceFact{
		UHostID:             id,
		Name:                name,
		State:               state,
		ChargeType:          chargeType,
		IsSpot:              isSpot,
		GpuType:             gpuType,
		GPU:                 int(gpu),
		InstancePrice:       instancePrice,
		DiskPrice:           diskPrice,
		ImagePrice:          imagePrice,
		HasInstancePrice:    hasInstancePrice,
		HasDiskPrice:        hasDiskPrice,
		HasImagePrice:       hasImagePrice,
		ActualComputeCharge: actualInstanceCost(state, chargeType, isSpot, instancePrice),
		Period:              billingPeriod(chargeType, isSpot),
		Region:              region,
		Zone:                zone,
	}
	fact.Components, fact.HasDiskBreakdown, fact.HasPowerOffBreakdown = billingCostComponents(host, fact)
	return fact
}

func billingCostComponents(host map[string]any, fact BillingInstanceFact) ([]BillingCostComponent, bool, bool) {
	components := make([]BillingCostComponent, 0, 5)
	components = append(components, BillingCostComponent{Kind: "compute", ChargeType: fact.ChargeType, Period: fact.Period, Price: fact.InstancePrice, Known: fact.HasInstancePrice})

	diskInfos := billingMapSlice(host["DiskPriceInfo"])
	powerOffInfos := billingMapSlice(host["PostPayPowerOffBillingResource"])
	if len(powerOffInfos) == 0 {
		powerOffInfos = billingMapSlice(host["PostPayPowerOffBillingResources"])
	}
	for _, info := range diskInfos {
		price, known := billingPriceField(info, "Price")
		chargeType, _ := info["ChargeType"].(string)
		isBoot, _ := info["IsBoot"].(bool)
		components = append(components, BillingCostComponent{
			Kind:                "disk",
			ChargeType:          chargeType,
			Period:              billingPeriod(chargeType, false),
			Price:               price,
			Known:               known,
			IsBoot:              isBoot,
			RetainedWhenStopped: billingPowerOffContains(powerOffInfos, isBoot, chargeType),
		})
	}
	if len(diskInfos) == 0 && fact.HasDiskPrice {
		components = append(components, BillingCostComponent{Kind: "disk_total", Period: "unknown", Price: fact.DiskPrice, Known: true})
	}
	components = append(components, BillingCostComponent{Kind: "image", ChargeType: fact.ChargeType, Period: fact.Period, Price: fact.ImagePrice, Known: fact.HasImagePrice})
	return components, len(diskInfos) > 0, len(powerOffInfos) > 0
}

func billingMapSlice(v any) []map[string]any {
	rows, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		if m, ok := row.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

func billingPowerOffContains(rows []map[string]any, isBoot bool, chargeType string) bool {
	for _, row := range rows {
		rowBoot, _ := row["IsBoot"].(bool)
		rowCharge, _ := row["ChargeType"].(string)
		price, known := billingPriceField(row, "Price")
		if rowBoot == isBoot && rowCharge == chargeType && known && price > 0 {
			return true
		}
	}
	return false
}

func accumulateCurrentCosts(summary *BillingFactsSummary, fact BillingInstanceFact) {
	switch fact.State {
	case "Running":
		for _, component := range fact.Components {
			if !component.Known || (component.Period == "unknown" && component.Price != 0) {
				summary.HasUnknownCurrentCost = true
				continue
			}
			if component.Period == "unknown" {
				continue
			}
			summary.CurrentTotals[component.Period] += component.Price
			if component.Period == "hour" {
				summary.HourlyTotal += component.Price
			}
		}
	case "Stopped":
		if fact.isHourly() {
			for _, component := range fact.Components {
				if component.Kind != "disk" || !component.RetainedWhenStopped {
					continue
				}
				if !component.Known || component.Period == "unknown" {
					summary.HasUnknownCurrentCost = true
					continue
				}
				summary.CurrentTotals[component.Period] += component.Price
				summary.StoppedRetainedTotal += component.Price
				if component.Period == "hour" {
					summary.HourlyTotal += component.Price
				}
			}
			if !fact.HasPowerOffBreakdown && (fact.HasDiskPrice || fact.HasDiskBreakdown) {
				for _, component := range fact.Components {
					if (component.Kind == "disk" || component.Kind == "disk_total") && component.Known && component.Price > 0 {
						summary.HasUnknownStoppedRetained = true
						summary.HasUnknownCurrentCost = true
						break
					}
				}
			}
			if fact.HasImagePrice && fact.ImagePrice > 0 {
				summary.HasUnknownCurrentCost = true
			}
		}
	default:
		summary.HasUnknownCurrentCost = true
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

func billingIntegerField(host map[string]any, key string) (int, bool) {
	v, ok := billingPriceField(host, key)
	return int(v), ok
}

// isHourly accepts both current Postpay and legacy Dynamic records. Spot is
// detected via IsSpot because upstream does not emit ChargeType "Spot".
func (fact BillingInstanceFact) isHourly() bool {
	return fact.ChargeType == "Postpay" || fact.ChargeType == "Dynamic" || fact.IsSpot
}

// actualInstanceCost projects only the compute portion. The describe API returns
// its configured unit quote regardless of power state; a stopped hourly instance
// has no running compute charge. Retained resources are never inferred here and
// come only from PostPayPowerOffBillingResource.
func actualInstanceCost(state, chargeType string, isSpot bool, price float64) float64 {
	if state == "Stopped" && (chargeType == "Postpay" || chargeType == "Dynamic" || isSpot) {
		return 0
	}
	return price
}

func formatInstanceFactCost(fact BillingInstanceFact) string {
	billing := chargeTypeLabel(fact.ChargeType, fact.IsSpot)
	location := strings.Trim(strings.Join([]string{fact.Region, fact.Zone}, "/"), "/")
	if location == "" {
		location = "区域未返回"
	}
	lines := []string{fmt.Sprintf("- %s (%s, %s, %s×%d, %s, %s)", fact.UHostID, fact.Name, location, fact.GpuType, fact.GPU, fact.State, billing)}
	for _, component := range fact.Components {
		label := billingComponentLabel(component)
		if !component.Known {
			lines = append(lines, "  - "+label+"：未返回")
			continue
		}
		value := fmt.Sprintf("¥%.2f%s", component.Price, billingUnitSuffix(component.Period))
		if component.Kind == "compute" && fact.State == "Stopped" && fact.isHourly() {
			value = fmt.Sprintf("当前 ¥0；开机配置报价 ¥%.2f%s", component.Price, billingUnitSuffix(component.Period))
		}
		if component.RetainedWhenStopped && fact.State == "Stopped" {
			value += "（关机后仍计费）"
		}
		if component.Kind == "disk_total" {
			value += "（上游未分拆系统盘/数据盘或计费周期，不纳入合计）"
		}
		lines = append(lines, "  - "+label+"："+value)
	}
	return strings.Join(lines, "\n")
}

func billingComponentLabel(component BillingCostComponent) string {
	switch component.Kind {
	case "compute":
		return "算力"
	case "image":
		return "镜像"
	case "disk_total":
		return "磁盘汇总"
	case "disk":
		if component.IsBoot {
			return "系统盘"
		}
		return "数据盘/CVolume"
	default:
		return component.Kind
	}
}

func billingUnitSuffix(period string) string {
	switch period {
	case "hour":
		return "/时"
	case "day":
		return "/天"
	case "month":
		return "/月"
	case "year":
		return "/年"
	default:
		return "（周期未返回）"
	}
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
	case "Dynamic":
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
