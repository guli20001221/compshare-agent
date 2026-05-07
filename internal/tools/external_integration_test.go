package tools

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/compshare-agent/internal/config"
)

func TestRealExternalExecutor_GetCompShareInstanceMonitor(t *testing.T) {
	if os.Getenv("RUN_REAL_MONITOR_TEST") != "1" {
		t.Skip("set RUN_REAL_MONITOR_TEST=1 to call the real CompShare API")
	}

	cfgPath := os.Getenv("REAL_AGENT_CONFIG")
	if cfgPath == "" {
		cfgPath = "deploy/conf/agent.yaml"
	}
	cfgPath = resolveTestConfigPath(t, cfgPath)

	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	exec := NewExternalExecutor(cfg.Agent)
	ctx := context.Background()

	instances, err := exec.Execute(ctx, "DescribeCompShareInstance", map[string]any{"Limit": 100})
	if err != nil {
		t.Fatalf("DescribeCompShareInstance failed: %v", err)
	}

	hosts, _ := instances["UHostSet"].([]any)
	if len(hosts) == 0 {
		t.Skip("real account has no instances to monitor")
	}
	host, ok := hosts[0].(map[string]any)
	if !ok {
		t.Fatalf("unexpected UHostSet[0] type %T", hosts[0])
	}
	uhostID, _ := host["UHostId"].(string)
	if uhostID == "" {
		t.Fatalf("first instance has no UHostId: %#v", host)
	}

	endTime := time.Now().Unix()
	startTime := endTime - 300
	monitor, err := exec.Execute(ctx, "GetCompShareInstanceMonitor", map[string]any{
		"UHostIds":  []any{uhostID},
		"StartTime": float64(startTime),
		"EndTime":   float64(endTime),
	})
	if err != nil {
		t.Fatalf("GetCompShareInstanceMonitor failed for %s: %v", uhostID, err)
	}

	data, _ := monitor["Data"].(map[string]any)
	list, _ := data["List"].([]any)
	if len(list) == 0 {
		t.Fatalf("monitor response contains no Data.List entries for %s: %#v", uhostID, monitor)
	}
	entry, ok := list[0].(map[string]any)
	if !ok {
		t.Fatalf("unexpected Data.List[0] type %T", list[0])
	}
	if got, _ := entry["UHostId"].(string); got != uhostID {
		t.Fatalf("monitor UHostId = %q, want %q", got, uhostID)
	}
}

func resolveTestConfigPath(t *testing.T, path string) string {
	t.Helper()
	if filepath.IsAbs(path) {
		return path
	}

	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("get working directory: %v", err)
	}
	for dir := wd; ; dir = filepath.Dir(dir) {
		candidate := filepath.Join(dir, path)
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
	}
	return path
}
