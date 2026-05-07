package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/compshare-agent/internal/config"
)

// TestProbeSpecificInstances dumps full DescribeCompShareInstance fields for
// a target list of UHostIds. Gated by RUN_PROBE=1 + PROBE_CONFIG=<yaml-path>.
// Used to investigate cases where the agent's monitor narrative reports
// "no data" so we can distinguish platform-side vs guest-side root causes.
func TestProbeSpecificInstances(t *testing.T) {
	if os.Getenv("RUN_PROBE") != "1" {
		t.Skip("set RUN_PROBE=1 to run the real-account probe")
	}
	cfgPath := os.Getenv("PROBE_CONFIG")
	if cfgPath == "" {
		t.Fatal("set PROBE_CONFIG=<path-to-agent.yaml>")
	}
	cfgPath = resolveTestConfigPath(t, cfgPath)
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	exec := NewExternalExecutor(cfg.Agent)

	targets := strings.Split(os.Getenv("PROBE_UHOST_IDS"), ",")
	if len(targets) == 0 || (len(targets) == 1 && targets[0] == "") {
		t.Fatal("set PROBE_UHOST_IDS=uhost-a,uhost-b,...")
	}

	args := map[string]any{"Limit": 100}
	if v := os.Getenv("PROBE_FILTER_UHOSTIDS"); v == "1" {
		ids := []any{}
		for _, t := range targets {
			t = strings.TrimSpace(t)
			if t != "" {
				ids = append(ids, t)
			}
		}
		args["UHostIds"] = ids
	}
	result, err := exec.Execute(context.Background(), "DescribeCompShareInstance", args)
	if err != nil {
		t.Fatalf("DescribeCompShareInstance: %v", err)
	}

	hosts, _ := result["UHostSet"].([]any)
	if os.Getenv("PROBE_LIST_ALL") == "1" {
		fmt.Printf("=== ALL INSTANCES (count=%d) ===\n", len(hosts))
		for _, h := range hosts {
			host, _ := h.(map[string]any)
			id, _ := host["UHostId"].(string)
			name, _ := host["Name"].(string)
			state, _ := host["State"].(string)
			fmt.Printf("  %s  %-30s  %s\n", id, name, state)
		}
		fmt.Println()
	}
	hit := map[string]bool{}
	for _, h := range hosts {
		host, ok := h.(map[string]any)
		if !ok {
			continue
		}
		id, _ := host["UHostId"].(string)
		for _, target := range targets {
			target = strings.TrimSpace(target)
			if id != target {
				continue
			}
			hit[id] = true
			compact := map[string]any{
				"UHostId":                  host["UHostId"],
				"Name":                     host["Name"],
				"State":                    host["State"],
				"OsType":                   host["OsType"],
				"OsName":                   host["OsName"],
				"GPU":                      host["GPU"],
				"GpuType":                  host["GpuType"],
				"CompShareImageType":       host["CompShareImageType"],
				"CompShareImageName":       host["CompShareImageName"],
				"CompShareImageOwnerAlias": host["CompShareImageOwnerAlias"],
				"InstanceType":             host["InstanceType"],
				"MachineType":              host["MachineType"],
				"ChargeType":               host["ChargeType"],
				"CreateTime":               host["CreateTime"],
				"StartTime":                host["StartTime"],
				"StopTime":                 host["StopTime"],
				"UpdateTime":               host["UpdateTime"],
				"SupportWithoutGpuStart":   host["SupportWithoutGpuStart"],
				"MonitorMessages":          host["MonitorMessages"],
			}
			b, _ := json.MarshalIndent(compact, "", "  ")
			fmt.Printf("=== %s ===\n%s\n\n", id, string(b))
		}
	}
	for _, target := range targets {
		target = strings.TrimSpace(target)
		if !hit[target] {
			fmt.Printf("=== %s ===\nNOT FOUND in DescribeCompShareInstance result\n\n", target)
		}
	}
}
