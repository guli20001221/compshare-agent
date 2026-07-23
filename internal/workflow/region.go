package workflow

import (
	"fmt"
	"strings"
)

// defaultRegion is the Region paired with defaultZone. Kept in lockstep with
// defaultZone (cn-wlcb-01 → cn-wlcb) so workflow fallbacks are consistent.
//
// This is the workflow-side fallback only; the runtime Region for mutating
// workflows comes from the queried instance via extractInstanceRegion.
// It is independent of config.yaml's `cfg.Region`, which only matters for
// CLI single-region dev mode — see internal/tools/external.go for that
// path. The two values do not need to match in production (HTTP path).
//
// CreateInstanceWorkflow does NOT currently use this fallback; its read
// tools are gated by SafeToolExecutor.filterSafeArgs which would drop any
// args["Region"] (registry schema in internal/tools/registry.go does not
// declare Region for those tools). See PR-β1 follow-up.
const defaultRegion = "cn-wlcb"

// regionFromZone derives a Region name from a Zone name by stripping the
// trailing "-<index>" segment. CompShare zone naming is "<region>-<index>"
// where <region> itself contains at least one dash (e.g. "cn-sh2-02" →
// "cn-sh2", "cn-wlcb-01" → "cn-wlcb"). Returns "" when the input clearly
// is not a Zone — empty string, no separator, or fewer than 2 dashes (which
// guards against a caller accidentally passing a Region like "cn-wlcb" and
// getting "cn" back).
//
// This is a derivation fallback only. When the upstream response carries an
// explicit Region field, prefer that — see extractInstanceRegion.
func regionFromZone(zone string) string {
	zone = strings.TrimSpace(zone)
	if zone == "" {
		return ""
	}
	if strings.Count(zone, "-") < 2 {
		return ""
	}
	idx := strings.LastIndex(zone, "-")
	if idx <= 0 {
		return ""
	}
	return zone[:idx]
}

// extractInstanceRegion returns the Region the workflow should use for a
// mutating call on a queried instance. Resolution order:
//  1. Region field from the first UHostSet entry (upstream populates this).
//  2. regionFromZone(Zone) derived from the same entry.
//  3. defaultRegion (CLI/dev fallback).
//
// This pairs with extractInstanceZone — call both when building args for a
// mutating step so the upstream signer does not have to reverse-derive Region
// from Zone in a code path that only runs in IsInternalCall() mode.
func extractInstanceRegion(result map[string]any, defaultRegionVal string) string {
	if result != nil {
		if hostSet, ok := result["UHostSet"].([]any); ok && len(hostSet) > 0 {
			if first, ok := hostSet[0].(map[string]any); ok {
				if region, ok := first["Region"].(string); ok && region != "" {
					return region
				}
				if zone, ok := first["Zone"].(string); ok && zone != "" {
					if derived := regionFromZone(zone); derived != "" {
						return derived
					}
				}
			}
		}
	}
	return defaultRegionVal
}

func extractRequiredInstanceLocation(result map[string]any) (region, zone string, err error) {
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
	if region == "" && zone != "" {
		region = regionFromZone(zone)
	}
	if zone == "" || region == "" {
		return "", "", fmt.Errorf("未获取到实例真实可用区，无法安全执行该操作。请稍后重试或到控制台确认实例可用区。")
	}
	return region, zone, nil
}

// stepQuerySupportZones queries DescribeCompShareSupportZone so a mutating
// step can resolve the catalog-backed zone_id/az_group a Pod/Container
// instance's underlying API requires (addRequiredPodPlacementArgs). The
// UHostSet the same workflow's DescribeCompShareInstance step returns never
// carries usable ZoneId/RegionId — upstream tags them json:"-" — so this
// catalog lookup keyed by the Zone string is the only real resolution path.
// Optional: a failed/slow lookup must not block a VM-only workflow that
// doesn't need the numeric IDs; addRequiredPodPlacementArgs fails closed on
// its own once it establishes the target actually is Pod/Container.
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
	host, ok := firstInstance(result)
	if !ok {
		return nil, fmt.Errorf("未获取到实例真实可用区，无法安全执行该操作。请稍后重试或到控制台确认实例可用区。")
	}
	zone := strings.TrimSpace(stringFieldAny(host["Zone"]))
	region := strings.TrimSpace(stringFieldAny(host["Region"]))
	if zone == "" {
		return nil, fmt.Errorf("未获取到实例真实可用区，无法安全执行该操作。请稍后重试或到控制台确认实例可用区。")
	}
	if sz, ok := supportZonePlacementForZone(supportZones, zone); ok {
		if region == "" {
			region = sz.region
		}
		if sz.zoneID != 0 {
			args["zone_id"] = sz.zoneID
		}
		if sz.azGroup != 0 {
			args["az_group"] = sz.azGroup
		}
	}
	if region == "" {
		if derived := regionFromZone(zone); derived != "" {
			region = derived
		}
	}
	if region == "" {
		return nil, fmt.Errorf("未获取到实例真实可用区，无法安全执行该操作。请稍后重试或到控制台确认实例可用区。")
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
		if strings.TrimSpace(stringFieldAny(entry["Zone"])) != zone {
			continue
		}
		placement := supportZonePlacement{
			region: stringFieldAny(entry["Region"]),
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
