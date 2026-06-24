package intent

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/compshare-agent/internal/config"
	"github.com/compshare-agent/internal/tools"
)

// TestPricingProbe_RootCause probes GetCompShareInstancePrice across a
// GPU x Zone matrix (bypassing the planner) to disambiguate WHY the routing
// pricing handler's price call fails for 5090: is cn-sh2-02 simply not a valid
// pricing zone for ANY gpu (static vocabulary gap), or is it a per-(gpu,zone)
// availability/inventory thing? Live, env-gated: needs creds + PRICING_PROBE=1.
//
//	PRICING_PROBE=1 COMPSHARE_PROJECT_ID=org-... <STS creds> \
//	  go test ./internal/intent -run TestPricingProbe_RootCause -v -count=1
func TestPricingProbe_RootCause(t *testing.T) {
	if os.Getenv("PRICING_PROBE") != "1" {
		t.Skip("set PRICING_PROBE=1 + live creds to run the pricing root-cause probe")
	}
	cfg, err := config.Load("../../deploy/conf/config.yaml")
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	exec := tools.NewExternalExecutor(cfg.Agent)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	ctx = tools.WithUser(ctx, tools.UserContext{
		RoleUrn:     cfg.Agent.STS.DefaultRoleUrn,
		SessionName: cfg.Agent.STS.DefaultSessionName,
		ProjectId:   cfg.Agent.ProjectId,
		Region:      cfg.Agent.Region,
	})

	describe, err := exec.Execute(ctx, "DescribeAvailableCompShareInstanceTypes", map[string]any{})
	if err != nil {
		t.Fatalf("Describe FAILED (auth/wiring, not the price bug): %v", err)
	}
	items := mapSliceAt(describe, "AvailableInstanceTypes")
	t.Logf("Describe OK: %d instance types", len(items))

	// What zone does Describe list each GPU in?
	for _, gpu := range []string{"5090", "4090", "A100"} {
		t.Logf("Describe entry %s: %s", gpu, dumpPricingEntry(gpu, items))
	}

	zones := []string{"cn-sh2-02", "cn-wlcb-01", "cn-qz-01", "cn-bj2-02"}
	for _, gpu := range []string{"5090", "4090", "A100"} {
		spec := pickDefaultPricingSpec(gpu, items)
		if spec.Cpu == 0 || spec.Memory == 0 {
			t.Logf("---- %s: spec incomplete (Cpu=%d Mem=%dGB), skipping ----", gpu, spec.Cpu, spec.Memory)
			continue
		}
		t.Logf("---- %s  (Describe zone=%q, spec Cpu=%d Mem=%dGB) ----", gpu, spec.Zone, spec.Cpu, spec.Memory)
		// Leg 1: OMIT Zone entirely. Agent-1 source read says the host price
		// path has no default-zone fallback (RegionId stays 0); this resolves
		// empirically whether omit-Zone returns a real price, errors, or zeros.
		omitArgs := map[string]any{
			"GpuType": gpu, "Gpu": 1, "Cpu": spec.Cpu, "Memory": spec.Memory * 1024,
		}
		if raw, perr := exec.Execute(ctx, "GetCompShareInstancePrice", omitArgs); perr != nil {
			t.Logf("   [%s] zone=<OMITTED>   FAIL: %v", gpu, perr)
		} else {
			t.Logf("   [%s] zone=<OMITTED>   OK: %s", gpu, dumpPriceBilling(raw))
		}
		// Leg 2: explicit zone matrix — capture actual prices to confirm both
		// (a) which zones the validator accepts and (b) whether the catalog
		// price is zone-uniform across accepted zones.
		for _, zone := range zones {
			args := map[string]any{
				"Zone": zone, "GpuType": gpu, "Gpu": 1, "Cpu": spec.Cpu, "Memory": spec.Memory * 1024,
			}
			if raw, perr := exec.Execute(ctx, "GetCompShareInstancePrice", args); perr != nil {
				t.Logf("   [%s] zone=%-12s FAIL: %v", gpu, zone, perr)
			} else {
				t.Logf("   [%s] zone=%-12s OK: %s", gpu, zone, dumpPriceBilling(raw))
			}
		}
	}
}

func dumpPricingEntry(gpuName string, items []any) string {
	for _, item := range items {
		entry, ok := item.(map[string]any)
		if !ok || safeString(entry, "Name") != gpuName {
			continue
		}
		out := map[string]any{"Name": entry["Name"], "Zone": entry["Zone"]}
		if sizes := mapSliceAt(entry, "MachineSizes"); len(sizes) > 0 {
			out["MachineSizes_count"] = len(sizes)
		}
		b, _ := json.Marshal(out)
		return string(b)
	}
	return "(no entry named " + gpuName + ")"
}

// dumpPriceBilling renders the per-charge-type price map extracted from a raw
// GetCompShareInstancePrice response, so the probe log can be eyeballed for
// zone-uniformity and for whether omit-Zone yields a real (non-empty) price.
func dumpPriceBilling(raw map[string]any) string {
	bill := pricingBillingTable(raw)
	if len(bill) == 0 {
		return "(no billing rows extracted)"
	}
	b, _ := json.Marshal(bill)
	return string(b)
}
