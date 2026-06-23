package engine

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/compshare-agent/internal/config"
	"github.com/compshare-agent/internal/tools"
)

// TestChargeTypePostpayProbe — live, env-gated (CHARGE_PROBE=1): does
// CheckCompShareResourceCapacity accept ChargeType="Postpay" (the platform-wide
// 按量 value after #246) the same as the legacy "Dynamic"? Verifies before the
// deploy-capacity / stock-precheck sites flip Dynamic→Postpay. Read-only.
func TestChargeTypePostpayProbe(t *testing.T) {
	if os.Getenv("CHARGE_PROBE") != "1" {
		t.Skip("set CHARGE_PROBE=1 + live creds to run")
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

	// cuda128_torch291_py312 (compshareImage-1minbz219ceq) on 4090 in cn-wlcb-01
	// — a real, queryable combo from the 2026-06-11 catalog probe.
	base := func(charge string) map[string]any {
		return map[string]any{
			"Zone": "cn-wlcb-01", "GpuType": "4090", "MachineType": "G",
			"MinimalCpuPlatform": "Auto", "CompShareImageId": "compshareImage-1minbz219ceq",
			"ChargeType": charge,
			"Disks":      []any{map[string]any{"IsBoot": true, "Type": "CLOUD_SSD", "Size": 60}},
		}
	}
	for _, charge := range []string{"Dynamic", "Postpay"} {
		res, derr := exec.Execute(ctx, "CheckCompShareResourceCapacity", base(charge))
		if derr != nil {
			t.Logf("ChargeType=%-8s FAIL: %v", charge, derr)
			continue
		}
		specs, _ := res["Specs"].([]any)
		t.Logf("ChargeType=%-8s OK, Specs len=%d", charge, len(specs))
	}
}
