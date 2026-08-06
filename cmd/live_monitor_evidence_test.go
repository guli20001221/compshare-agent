package main

// Configured-model acceptance for the A-class monitor path. Monitor results are
// represented to the Agent by an evidence envelope; the renderer's text is not
// appended to the final answer. The fixed API snapshot makes the assertion
// repeatable while the configured answer model is real.
//
// Run:
//
//  go test ./cmd -run TestLiveMonitorEnvelopeAnswer -count=1 -v -timeout 5m \
//    -live-monitor-evidence

import (
	"context"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/compshare-agent/internal/config"
	"github.com/compshare-agent/internal/engine"
	"github.com/compshare-agent/internal/entity"
	"github.com/compshare-agent/internal/envelope"
	"github.com/compshare-agent/internal/readprojection"
	"github.com/stretchr/testify/require"
)

var liveMonitorEvidence = flag.Bool("live-monitor-evidence", false, "run configured-model monitor-envelope acceptance; off = skip")

const monitorEvidenceAcceptanceID = "uhost-1t09vtnm0qyj"

func TestLiveMonitorEnvelopeAnswer(t *testing.T) {
	if !*liveMonitorEvidence {
		t.Skip("set -live-monitor-evidence to run the configured-model acceptance")
	}

	root := behavioralRepoRoot(t)
	if originalDir, err := os.Getwd(); err == nil {
		require.NoError(t, os.Chdir(root))
		t.Cleanup(func() { _ = os.Chdir(originalDir) })
	}
	cfg, err := config.Load(filepath.Join(root, "deploy", "conf", "config.local.yaml"))
	require.NoError(t, err)
	if strings.TrimSpace(cfg.Agent.LLM.APIKey) == "" {
		t.Skip("agent.llm.api_key is empty; cannot run the configured-model acceptance")
	}
	deps, err := engine.NewSharedDeps(cfg)
	require.NoError(t, err)
	deps.ExternalExecutor = monitorEvidenceAcceptanceExecutor{}

	record := runCaseInProcess(context.Background(), deps, false, "live-monitor-evidence", "live-monitor-evidence", nil,
		[]string{"请查询实例 ID " + monitorEvidenceAcceptanceID + " 当前 GPU 占用多少。"}, 3*time.Minute)
	t.Logf("actions=%v reply=%q", allStepActions(record), record.FinalReply)
	require.Empty(t, record.Error, "the configured model must complete the monitor turn")
	require.True(t, allStepActions(record)["GetCompShareInstanceMonitor"],
		"the Agent must query the monitor instead of answering from a guess")

	observed := readprojection.BuildMonitorEnvelope(
		[]entity.InstanceSnapshot{{UHostId: monitorEvidenceAcceptanceID, Name: "monitor-acceptance", State: "Running", GpuType: "4090", GPU: 1}},
		[]readprojection.Metric{readprojection.MetricGPU}, monitorEvidenceAcceptanceSnapshot(),
	)
	facts := liveGPUFacts(observed)
	require.NotEmpty(t, facts, "fixture premise: the monitor call exposes an API-backed GPU fact")
	require.True(t, liveMonitorReplyRepresentsFacts(record.FinalReply, facts),
		"the Agent reply must state the observed GPU percentage without inventing another value")
}

// monitorEvidenceAcceptanceExecutor lets the real Agent exercise its normal
// typed capability and envelope path, while refusing every unrelated action.
// It has no credential and cannot mutate a platform account.
type monitorEvidenceAcceptanceExecutor struct{}

func (monitorEvidenceAcceptanceExecutor) Execute(_ context.Context, action string, _ map[string]any) (map[string]any, error) {
	switch action {
	case "DescribeCompShareInstance":
		return map[string]any{"TotalCount": float64(1), "UHostSet": []any{map[string]any{
			"UHostId": monitorEvidenceAcceptanceID, "Name": "monitor-acceptance", "State": "Running",
			"GpuType": "4090", "GPU": float64(1), "CPU": float64(16), "Memory": float64(65536),
		}}}, nil
	case "GetCompShareInstanceMonitor":
		return monitorEvidenceAcceptanceSnapshot(), nil
	default:
		return nil, fmt.Errorf("monitor evidence acceptance refuses unexpected action %q", action)
	}
}

func monitorEvidenceAcceptanceSnapshot() map[string]any {
	return map[string]any{"Data": map[string]any{"List": []any{map[string]any{
		"UHostId": monitorEvidenceAcceptanceID,
		"Metrics": []any{map[string]any{
			"MetricKey": "cloudwatch_gpu_util",
			"Results": []any{map[string]any{"Values": []any{map[string]any{
				"Value": float64(42.75), "Timestamp": float64(1778420000),
			}}}},
		}},
	}}}}
}

func liveGPUFacts(observed envelope.Envelope) []envelope.Fact {
	var facts []envelope.Fact
	for _, fact := range observed.Facts {
		if fact.Source == envelope.FactSourceAPI && (fact.Key == "gpu_usage" || strings.HasPrefix(fact.Key, "gpu_usage.")) {
			facts = append(facts, fact)
		}
	}
	return facts
}

var liveMonitorPercentPattern = regexp.MustCompile(`(-?\d+(?:\.\d+)?)\s*%`)

// liveMonitorReplyRepresentsFacts permits ordinary decimal rounding (for
// example, 42.75% -> 42.8% or 43%) but requires a numeric GPU answer. It does
// not prescribe wording, list shape, or renderer-owned answer text.
func liveMonitorReplyRepresentsFacts(reply string, facts []envelope.Fact) bool {
	if !strings.Contains(strings.ToLower(reply), "gpu") && !strings.Contains(reply, "显卡") {
		return false
	}
	var percentages []float64
	for _, match := range liveMonitorPercentPattern.FindAllStringSubmatch(strings.ReplaceAll(reply, "％", "%"), -1) {
		value, err := strconv.ParseFloat(match[1], 64)
		if err == nil {
			percentages = append(percentages, value)
		}
	}
	if len(percentages) == 0 {
		return false
	}
	for _, fact := range facts {
		if fact.Unit != "%" {
			return false
		}
		want, err := strconv.ParseFloat(strings.TrimSpace(fmt.Sprint(fact.Value)), 64)
		if err != nil {
			return false
		}
		matched := false
		for _, got := range percentages {
			if math.Abs(got-want) <= 0.51 { // conventional rounding to a whole percentage point
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	return true
}
