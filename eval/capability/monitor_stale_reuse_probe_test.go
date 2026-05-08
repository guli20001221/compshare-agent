package capability

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/compshare-agent/internal/config"
	"github.com/compshare-agent/internal/engine"
	"github.com/compshare-agent/internal/security"
	"github.com/compshare-agent/internal/tools"
)

type monitorProbeEvent struct {
	Type   string
	Action string
	Args   map[string]any
}

type monitorProbeCase struct {
	ID       string
	First    string
	FollowUp string
}

type monitorProbeResult struct {
	CaseID             string
	FirstMonitorCalls  int
	SecondMonitorCalls int
	FirstActions       []string
	SecondActions      []string
	FirstReply         string
	SecondReply        string
	Classification     string
	Error              string
}

func TestProbeMonitorStaleReuse(t *testing.T) {
	if os.Getenv("RUN_MONITOR_STALE_REUSE_PROBE") != "1" {
		t.Skip("set RUN_MONITOR_STALE_REUSE_PROBE=1 to run real-account monitor stale-reuse probe")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()

	cfg := probeConfigFromEnv(t)
	target := pickMonitorProbeTarget(ctx, t, cfg)
	artifactPath := os.Getenv("MONITOR_STALE_REUSE_ARTIFACT")
	if artifactPath == "" {
		artifactPath = filepath.Join("eval", "capability", "2026-05-08-ds-v4-flash-monitor-stale-reuse-probe.md")
	}
	artifactPath = resolveRepoRelativePath(t, artifactPath)

	cases := []monitorProbeCase{
		{
			ID:       "adjacent_same_metric",
			First:    fmt.Sprintf("帮我看下 %s 这台机器当前的 CPU、内存、GPU 利用率和显存", target.QueryName),
			FollowUp: "只看刚才那台机器的 GPU 和显存监控",
		},
		{
			ID:       "adjacent_explicit_refresh",
			First:    fmt.Sprintf("帮我看下 %s 这台机器当前的 CPU、内存、GPU 利用率和显存", target.QueryName),
			FollowUp: "重新查一下刚才那台机器现在的 GPU 利用率和显存，不要复用上一轮",
		},
		{
			ID:       "adjacent_pronoun_now",
			First:    fmt.Sprintf("帮我看下 %s 这台机器当前的 CPU、内存、GPU 利用率和显存", target.QueryName),
			FollowUp: "它现在 GPU 和显存是多少？",
		},
	}

	var results []monitorProbeResult
	for _, tc := range cases {
		results = append(results, runMonitorProbeCase(ctx, cfg, tc))
	}

	writeMonitorProbeArtifact(t, artifactPath, cfg, target, results)
	for _, r := range results {
		if r.Error != "" {
			t.Logf("%s error: %s", r.CaseID, r.Error)
			continue
		}
		t.Logf("%s first_monitor_calls=%d second_monitor_calls=%d classification=%s",
			r.CaseID, r.FirstMonitorCalls, r.SecondMonitorCalls, r.Classification)
	}
}

type monitorProbeTarget struct {
	QueryName string
	UHostID   string
	Name      string
	State     string
	GPUType   string
	GPUCount  string
}

func probeConfigFromEnv(t *testing.T) *config.Config {
	t.Helper()
	required := []string{"COMPSHARE_PUBLIC_KEY", "COMPSHARE_PRIVATE_KEY", "LLM_API_KEY"}
	for _, key := range required {
		if os.Getenv(key) == "" {
			t.Fatalf("environment variable %s is required", key)
		}
	}
	region := os.Getenv("COMPSHARE_REGION")
	if region == "" {
		region = "cn-wlcb"
	}
	apiURL := os.Getenv("COMPSHARE_API_URL")
	if apiURL == "" {
		apiURL = "https://api.compshare.cn/"
	}
	baseURL := os.Getenv("LLM_BASE_URL")
	if baseURL == "" {
		baseURL = "https://api.modelverse.cn/v1"
	}
	model := os.Getenv("LLM_MODEL")
	if model == "" {
		model = "deepseek-v4-flash"
	}

	return &config.Config{Agent: config.AgentConfig{
		Executor:        "external",
		CompShareAPIURL: apiURL,
		PublicKey:       os.Getenv("COMPSHARE_PUBLIC_KEY"),
		PrivateKey:      os.Getenv("COMPSHARE_PRIVATE_KEY"),
		Region:          region,
		ProjectId:       os.Getenv("COMPSHARE_PROJECT_ID"),
		LLM:             config.LLMConfig{BaseURL: baseURL, APIKey: os.Getenv("LLM_API_KEY"), Model: model},
	}}
}

func pickMonitorProbeTarget(ctx context.Context, t *testing.T, cfg *config.Config) monitorProbeTarget {
	t.Helper()
	result, err := tools.NewExternalExecutor(cfg.Agent).Execute(ctx, "DescribeCompShareInstance", map[string]any{"Limit": 100})
	if err != nil {
		t.Fatalf("DescribeCompShareInstance: %v", err)
	}
	hosts, _ := result["UHostSet"].([]any)
	var fallback monitorProbeTarget
	for _, h := range hosts {
		host, ok := h.(map[string]any)
		if !ok {
			continue
		}
		target := monitorProbeTarget{
			UHostID:  stringField(host, "UHostId"),
			Name:     stringField(host, "Name"),
			State:    stringField(host, "State"),
			GPUType:  stringField(host, "GpuType"),
			GPUCount: fmt.Sprint(host["GPU"]),
		}
		if target.UHostID == "" {
			continue
		}
		target.QueryName = target.UHostID
		if fallback.UHostID == "" {
			fallback = target
		}
		if target.State == "Running" && target.GPUType != "" && !strings.HasPrefix(target.GPUCount, "0") {
			return target
		}
	}
	if fallback.UHostID != "" {
		return fallback
	}
	t.Fatal("no instances returned by DescribeCompShareInstance")
	return monitorProbeTarget{}
}

func runMonitorProbeCase(ctx context.Context, cfg *config.Config, tc monitorProbeCase) monitorProbeResult {
	eng := engine.New(cfg, nil)
	if _, err := eng.Init(ctx); err != nil {
		return monitorProbeResult{CaseID: tc.ID, Error: "Init: " + err.Error()}
	}

	firstEvents := []monitorProbeEvent{}
	firstReply, err := eng.Chat(ctx, tc.First, collectProbeEvents(&firstEvents))
	if err != nil {
		return monitorProbeResult{CaseID: tc.ID, Error: "first Chat: " + err.Error()}
	}

	secondEvents := []monitorProbeEvent{}
	secondReply, err := eng.Chat(ctx, tc.FollowUp, collectProbeEvents(&secondEvents))
	if err != nil {
		return monitorProbeResult{CaseID: tc.ID, Error: "second Chat: " + err.Error()}
	}

	r := monitorProbeResult{
		CaseID:             tc.ID,
		FirstMonitorCalls:  countToolCalls(firstEvents, "GetCompShareInstanceMonitor"),
		SecondMonitorCalls: countToolCalls(secondEvents, "GetCompShareInstanceMonitor"),
		FirstActions:       actionList(firstEvents),
		SecondActions:      actionList(secondEvents),
		FirstReply:         firstReply,
		SecondReply:        secondReply,
	}
	r.Classification = classifyMonitorProbe(r)
	return r
}

func collectProbeEvents(out *[]monitorProbeEvent) func(engine.StepEvent) {
	return func(ev engine.StepEvent) {
		if ev.Type != engine.StepToolCall && ev.Type != engine.StepToolResult && ev.Type != engine.StepError {
			return
		}
		*out = append(*out, monitorProbeEvent{
			Type:   fmt.Sprint(ev.Type),
			Action: ev.Action,
			Args:   ev.Args,
		})
	}
}

func countToolCalls(events []monitorProbeEvent, action string) int {
	n := 0
	for _, ev := range events {
		if ev.Action == action && ev.Type == fmt.Sprint(engine.StepToolCall) {
			n++
		}
	}
	return n
}

func actionList(events []monitorProbeEvent) []string {
	var out []string
	for _, ev := range events {
		if ev.Type == fmt.Sprint(engine.StepToolCall) {
			out = append(out, ev.Action)
		}
	}
	return out
}

var monitorMetricValueRE = regexp.MustCompile(`(?i)(GPU|CPU|显存|内存|利用率|monitor|监控).{0,30}(\d+(\.\d+)?\s*%|\d+(\.\d+)?)`)

func classifyMonitorProbe(r monitorProbeResult) string {
	if r.SecondMonitorCalls > 0 {
		return "FRESH_RECALLED"
	}
	if monitorMetricValueRE.MatchString(r.SecondReply) {
		return "STALE_REUSE_RISK"
	}
	return "NO_SECOND_MONITOR_CALL"
}

func writeMonitorProbeArtifact(t *testing.T, path string, cfg *config.Config, target monitorProbeTarget, results []monitorProbeResult) {
	t.Helper()
	var b strings.Builder
	b.WriteString("# ds v4 flash monitor stale-reuse probe\n\n")
	b.WriteString("- Date: 2026-05-08\n")
	b.WriteString("- Base URL: `" + cfg.Agent.LLM.BaseURL + "`\n")
	b.WriteString("- Model: `" + cfg.Agent.LLM.Model + "`\n")
	b.WriteString("- Method: real `engine.Chat()` two-turn conversations after `Engine.Init()`, with no object `tool_choice` on ds v4 flash because `SupportsObjectToolChoice=false`.\n")
	b.WriteString("- Target: `<target>` (State=" + target.State + ", GPUType=" + target.GPUType + ", GPU=" + target.GPUCount + ")\n\n")

	b.WriteString("## Decision table\n\n")
	b.WriteString("| Case | First monitor calls | Second monitor calls | Classification | First actions | Second actions |\n")
	b.WriteString("|---|---:|---:|---|---|---|\n")
	for _, r := range results {
		b.WriteString(fmt.Sprintf("| `%s` | %d | %d | `%s` | `%s` | `%s` |\n",
			r.CaseID, r.FirstMonitorCalls, r.SecondMonitorCalls, r.Classification,
			strings.Join(r.FirstActions, ", "), strings.Join(r.SecondActions, ", ")))
	}

	b.WriteString("\n## Case details\n\n")
	for _, r := range results {
		b.WriteString("### " + r.CaseID + "\n\n")
		if r.Error != "" {
			b.WriteString("- Error: `" + sanitizeArtifactText(r.Error, target) + "`\n\n")
			continue
		}
		b.WriteString("- First actions: `" + strings.Join(r.FirstActions, ", ") + "`\n")
		b.WriteString("- Second actions: `" + strings.Join(r.SecondActions, ", ") + "`\n")
		b.WriteString("- Classification: `" + r.Classification + "`\n\n")
		b.WriteString("First reply:\n\n```text\n" + sanitizeArtifactText(r.FirstReply, target) + "\n```\n\n")
		b.WriteString("Second reply:\n\n```text\n" + sanitizeArtifactText(r.SecondReply, target) + "\n```\n\n")
	}

	b.WriteString("## Interpretation\n\n")
	secondRecalls := 0
	staleRisks := 0
	for _, r := range results {
		if r.SecondMonitorCalls > 0 {
			secondRecalls++
		}
		if r.Classification == "STALE_REUSE_RISK" {
			staleRisks++
		}
	}
	b.WriteString(fmt.Sprintf("- Second-turn monitor recalls: %d/%d\n", secondRecalls, len(results)))
	b.WriteString(fmt.Sprintf("- Stale-reuse risk cases: %d/%d\n", staleRisks, len(results)))
	b.WriteString("- If `Second-turn monitor recalls` is 0 and replies contain concrete monitor values, ds v4 flash auto routing is reusing stale context and needs a non-object-tool-choice mitigation.\n")

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir artifact dir: %v", err)
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatalf("write artifact: %v", err)
	}
}

func resolveRepoRelativePath(t *testing.T, path string) string {
	t.Helper()
	if filepath.IsAbs(path) {
		return path
	}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("get working directory: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(wd, "go.mod")); err == nil {
			return filepath.Join(wd, path)
		}
		parent := filepath.Dir(wd)
		if parent == wd {
			t.Fatalf("could not locate repo root for artifact path %q", path)
		}
		wd = parent
	}
}

func sanitizeArtifactText(s string, target monitorProbeTarget) string {
	s = fmt.Sprint(security.RedactForTrace(s))
	for _, raw := range []string{target.UHostID, target.Name} {
		if raw != "" {
			s = strings.ReplaceAll(s, raw, "<target>")
		}
	}
	return s
}

func stringField(m map[string]any, key string) string {
	v, _ := m[key].(string)
	return v
}
