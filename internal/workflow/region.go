package workflow

import (
	"fmt"
	"strings"
)

// extractRequiredInstanceLocation returns a write-safe instance location.
// DescribeCompShareInstance carries the instance's Zone and Region. Some old
// or partially populated responses omit Region, so the matching row in the live
// DescribeCompShareSupportZone result is an accepted fallback. The location is
// never guessed from the Zone string and never replaced with a fixed default.
func extractRequiredInstanceLocation(result, supportZones map[string]any) (region, zone string, err error) {
	if result != nil {
		if hostSet, ok := result["UHostSet"].([]any); ok && len(hostSet) > 0 {
			if first, ok := hostSet[0].(map[string]any); ok {
				if v, ok := first["Zone"].(string); ok {
					zone = strings.TrimSpace(v)
				}
				if v, ok := first["Region"].(string); ok {
					region = strings.TrimSpace(v)
				}
			}
		}
	}
	if zone == "" {
		return "", "", fmt.Errorf("未获取到实例真实可用区，无法安全执行该操作。请稍后重试或到控制台确认实例可用区。")
	}
	if placement, ok := supportZonePlacementForZone(supportZones, zone); ok {
		if region == "" {
			region = placement.region
		} else if placement.region != "" && !strings.EqualFold(region, placement.region) {
			return "", "", fmt.Errorf("实例地域与实时可用区目录不一致，无法安全执行该操作。请稍后重试或到控制台确认实例位置。")
		}
	}
	if region == "" {
		return "", "", fmt.Errorf("未获取到实例真实地域，无法安全执行该操作。请稍后重试或到控制台确认实例位置。")
	}
	return region, zone, nil
}

// stepQuerySupportZones queries DescribeCompShareSupportZone so a mutating
// step can resolve the catalog-backed zone_id/az_group a Pod/Container
// instance's underlying API requires (addRequiredPodPlacementArgs). The
// UHostSet the same workflow's DescribeCompShareInstance step returns never
// carries usable ZoneId/RegionId — upstream tags them json:"-" — so this
// catalog lookup keyed by the Zone string is the only real resolution path.
// The lookup remains optional for VM operations whose instance response already
// carries Region. A missing lookup can never re-enable a guess: callers still
// fail closed when neither response supplies a real Region.
func stepQuerySupportZones() Step {
	return Step{
		Name:     "查询支持区",
		Type:     StepToolCall,
		Tool:     "DescribeCompShareSupportZone",
		Optional: true,
		BuildArgs: func(wfCtx *Context) (map[string]any, error) {
			return map[string]any{}, nil
		},
	}
}

func addRequiredPodPlacementArgs(args map[string]any, result map[string]any, supportZones map[string]any) (map[string]any, error) {
	_, ok := firstInstance(result)
	if !ok {
		return nil, fmt.Errorf("未获取到实例真实可用区，无法安全执行该操作。请稍后重试或到控制台确认实例可用区。")
	}
	region, zone, err := extractRequiredInstanceLocation(result, supportZones)
	if err != nil {
		return nil, err
	}
	if sz, ok := supportZonePlacementForZone(supportZones, zone); ok {
		if sz.zoneID != 0 {
			args["zone_id"] = sz.zoneID
		}
		if sz.azGroup != 0 {
			args["az_group"] = sz.azGroup
		}
	}
	args["Region"] = region
	args["Zone"] = zone
	addInstancePlacementArgsIfMissing(args, result)
	if resizeDiskViaInstance(result) {
		if _, ok := args["zone_id"]; !ok {
			return nil, fmt.Errorf("未获取到实例内部可用区编号，无法安全执行该操作。请稍后重试或到控制台确认实例可用区。")
		}
		if _, ok := args["az_group"]; !ok {
			return nil, fmt.Errorf("未获取到实例内部可用区编号，无法安全执行该操作。请稍后重试或到控制台确认实例可用区。")
		}
	}
	return args, nil
}

func addInstancePlacementArgsIfMissing(args map[string]any, result map[string]any) map[string]any {
	host, ok := firstInstance(result)
	if !ok {
		return args
	}
	if _, exists := args["zone_id"]; !exists {
		if id, ok := firstUint32Field(host, "ZoneId", "ZoneID", "zone_id"); ok && id != 0 {
			args["zone_id"] = id
		}
	}
	if _, exists := args["az_group"]; !exists {
		if id, ok := firstUint32Field(host, "RegionId", "RegionID", "region_id", "az_group", "AZGroup"); ok && id != 0 {
			args["az_group"] = id
		}
	}
	return args
}

type supportZonePlacement struct {
	region  string
	zoneID  uint32
	azGroup uint32
}

func supportZonePlacementForZone(result map[string]any, zone string) (supportZonePlacement, bool) {
	zone = strings.TrimSpace(zone)
	if result == nil || zone == "" {
		return supportZonePlacement{}, false
	}
	raw, _ := result["ZoneInfo"].([]any)
	for _, item := range raw {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(stringFieldAny(entry["Zone"])), zone) {
			continue
		}
		placement := supportZonePlacement{
			region: strings.TrimSpace(stringFieldAny(entry["Region"])),
		}
		if id, ok := parseUint32Any(entry["ZoneId"]); ok {
			placement.zoneID = id
		}
		if id, ok := parseUint32Any(entry["RegionId"]); ok {
			placement.azGroup = id
		}
		return placement, true
	}
	return supportZonePlacement{}, false
}

func stringFieldAny(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func firstUint32Field(m map[string]any, keys ...string) (uint32, bool) {
	for _, key := range keys {
		if id, ok := parseUint32Any(m[key]); ok {
			return id, true
		}
	}
	return 0, false
}

const missingWorkflowPriceMessage = "未获取到价格，无法安全确认，请稍后重试或到控制台确认。"

func requiredPriceField(result map[string]any, key string) (float64, error) {
	if result == nil {
		return 0, fmt.Errorf(missingWorkflowPriceMessage)
	}
	raw, exists := result[key]
	if exists {
		if price, ok := priceNumber(raw); ok {
			return price, nil
		}
	}
	for _, listKey := range []string{"PriceDetails", "OriginalPriceDetails", "ListPriceDetails"} {
		details, ok := result[listKey].([]any)
		if !ok {
			continue
		}
		for _, item := range details {
			row, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if price, ok := priceNumber(row[key]); ok {
				return price, nil
			}
		}
	}
	return 0, fmt.Errorf(missingWorkflowPriceMessage)
}
