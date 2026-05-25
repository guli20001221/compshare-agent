package workflow

import "strings"

// defaultRegion is the Region paired with defaultZone. Kept in lockstep with
// defaultZone (cn-wlcb-01 → cn-wlcb) so workflow fallbacks are consistent.
//
// This is the workflow-side fallback only; the runtime Region for mutating
// workflows comes from the queried instance via extractInstanceRegion.
// It is independent of agent.yaml's `cfg.Region`, which only matters for
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
