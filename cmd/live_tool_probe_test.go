package main

// End-to-end LIVE probe: real queries through the production wiring with the
// REAL CompShare executor.
//
// Why this exists: the RAG probes in internal/engine run the answer stack over
// &mockExecutor{}, whose Execute returns {"Action":..,"RetCode":0} for any
// unconfigured action — an empty success. So a question the production agent
// would answer from a tool (GPU specs, stock, instance state) comes back
// hedged there, and the hedge is a harness artifact, not a capability gap.
// This probe closes that gap: same deps as the HTTP server
// (configureSharedDepsFromEnv), same session setup (runCaseInProcess), plus a
// tenant identity in the context so tools.ExternalExecutor can AssumeRole and
// actually call api.compshare.cn.
//
// Read-only by construction: every confirmation is DECLINED (confirm=false, the
// production HTTP default), so no mutating workflow can execute even though the
// deploy config enables mutating tools.
//
// Real customer questions are never committed: the query list is read from
// -live-tool-queries and the transcript is written to -live-tool-out, both
// expected to live outside the repo.
//
// Run:
//
//	go test ./cmd -run TestLiveToolProbe -live-tool-probe -v -timeout 60m \
//	    -live-tool-queries <in.jsonl> -live-tool-out <out.jsonl> \
//	    -live-tool-top-org <id> -live-tool-org <id>

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/compshare-agent/internal/config"
	"github.com/compshare-agent/internal/engine"
	"github.com/compshare-agent/internal/observability"
	"github.com/compshare-agent/internal/tools"
)

var (
	liveToolProbe   = flag.Bool("live-tool-probe", false, "run the live end-to-end tool probe (real model + real CompShare API); off = skip")
	liveToolQueries = flag.String("live-tool-queries", "", "query JSONL: {case_id, query} or {case_id, turns:[{user}]} (required)")
	liveToolOut     = flag.String("live-tool-out", "", "write the transcript JSONL here (replay-output shape); empty = log only")
	liveToolConfig  = flag.String("live-tool-config", "", "config.yaml path; default deploy/conf/config.yaml")
	liveToolTopOrg  = flag.Uint("live-tool-top-org", 0, "top_organization_id for the STS role URN (required)")
	liveToolOrg     = flag.Uint("live-tool-org", 0, "organization_id (required)")
	liveToolEmail   = flag.String("live-tool-email", "", "user_email injected the way the gateway injects it (only some actions need it)")
	liveToolTimeout = flag.Duration("live-tool-timeout", 300*time.Second, "per-turn engine timeout")
	liveToolCases   = flag.Int("live-tool-cases", 0, "limit to the first N cases in file order; 0 = all")
)

func TestLiveToolProbe(t *testing.T) {
	if !*liveToolProbe {
		t.Skip("set -live-tool-probe to run (real model + real CompShare API)")
	}
	if *liveToolQueries == "" {
		t.Fatal("-live-tool-queries is required; real questions are never committed")
	}
	if *liveToolTopOrg == 0 || *liveToolOrg == 0 {
		t.Fatal("-live-tool-top-org and -live-tool-org are required (STS AssumeRole needs a tenant)")
	}

	root := behavioralRepoRoot(t)
	// Run from repo root so the corpus / kb-sidecar relative paths resolve the
	// way they do for the server binary.
	if orig, err := os.Getwd(); err == nil {
		if err := os.Chdir(root); err != nil {
			t.Fatalf("chdir repo root %s: %v", root, err)
		}
		t.Cleanup(func() { _ = os.Chdir(orig) })
	}

	cfgPath := orDefault(*liveToolConfig, filepath.Join(root, "deploy", "conf", "config.yaml"))
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("config.Load(%s): %v", cfgPath, err)
	}
	if cfg.Agent.LLM.APIKey == "" {
		t.Skip("agent.llm.api_key is empty; cannot run the live probe")
	}

	getenv := cfg.RuntimeGetenv(os.Getenv)
	deps, mutating, err := configureSharedDepsFromEnv(cfg, getenv)
	if err != nil {
		t.Fatalf("configureSharedDepsFromEnv: %v", err)
	}

	ctx, err := liveProbeUserContext(cfg, uint32(*liveToolTopOrg), uint32(*liveToolOrg), *liveToolEmail)
	if err != nil {
		t.Fatalf("build tenant context: %v", err)
	}
	t.Logf("wiring: model=%s mutating=%t rag_mode=%s sts=%t region=%s",
		cfg.Agent.LLM.Model, mutating, getenv("RAG_RETRIEVAL_MODE"), cfg.Agent.STS.ServiceAK != "", cfg.Agent.Region)

	cases := loadLiveToolQueries(t, *liveToolQueries)
	if len(cases) == 0 {
		t.Fatal("query list is empty")
	}
	if *liveToolCases > 0 && *liveToolCases < len(cases) {
		cases = cases[:*liveToolCases]
	}
	t.Logf("cases=%d from %s", len(cases), *liveToolQueries)

	records := make([]*replayCaseRecord, 0, len(cases))
	for i, c := range cases {
		t0 := time.Now()
		// Citation markers never reach the reply (stripped for display), so the
		// only place "which chunk did this answer claim" survives is the
		// retrieval trace. Capture it per case.
		var cited []string
		var searches int
		observe := func(eng *engine.Engine) {
			eng.SetRetrievalTraceObserver(func(tr observability.RetrievalTrace) {
				if len(tr.CitedChunkIDs) > 0 {
					cited = append(cited, tr.CitedChunkIDs...)
				}
				if len(tr.HitItems) > 0 || tr.Hits > 0 {
					searches++
				}
			})
		}
		// One rate-limit subject per case: the sample is N different users'
		// questions, not one user asking N times, and a shared subject would
		// trip user_turn_qps and return 请求过于频繁 instead of an answer.
		rec := runCaseInProcess(ctx, deps, mutating, "live-tool-probe:"+c.caseID, c.caseID, c.turns, *liveToolTimeout, observe)
		rec.CitedChunkIDs = dedupeStrings(cited)
		rec.RetrievalTraces = searches
		records = append(records, rec)

		acts := allStepActions(rec)
		names := make([]string, 0, len(acts))
		for a := range acts {
			names = append(names, a)
		}
		sort.Strings(names)
		reply := rec.FinalReply
		if rec.Error != "" {
			reply = "ERR: " + rec.Error
		}
		t.Logf("\n======== [%d/%d] %s (%dms) ========\n问：%s\n工具：%s\n引用：%s\n答：\n%s",
			i+1, len(cases), c.caseID, time.Since(t0).Milliseconds(),
			c.turns[0], strings.Join(names, ", "),
			orDefault(strings.Join(rec.CitedChunkIDs, ", "), "（无）"),
			truncateForLog(reply, 1200))
		if len(names) == 0 {
			t.Logf("  ⚠ 未调用任何工具（纯知识/生成回答）")
		}
		if rec.RetrievalTraces > 0 && len(rec.CitedChunkIDs) == 0 {
			t.Logf("  ⚠ 检索到证据但答案未标注引用（无法审计该答案是否有据）")
		}
	}

	if *liveToolOut != "" {
		if err := writeReplayJSONL(*liveToolOut, records); err != nil {
			t.Fatalf("write transcript: %v", err)
		}
		t.Logf("transcript -> %s", *liveToolOut)
	}
}

// liveProbeUserContext mirrors the identity the HTTP gateway injects: org ids
// from the request body, role URN derived from agent.sts.role_urn_template
// (agent.sts.default_role_urn wins when set), project/region from config.
func liveProbeUserContext(cfg *config.Config, topOrg, org uint32, email string) (context.Context, error) {
	roleUrn := cfg.Agent.STS.DefaultRoleUrn
	if roleUrn == "" {
		var err error
		roleUrn, err = tools.RoleUrnFromTemplate(cfg.Agent.STS.RoleUrnTemplate, topOrg)
		if err != nil {
			return nil, err
		}
	}
	return tools.WithUser(context.Background(), tools.UserContext{
		TopOrganizationID: topOrg,
		OrganizationID:    org,
		CompanyID:         topOrg,
		RoleUrn:           roleUrn,
		SessionName:       cfg.Agent.STS.DefaultSessionName,
		ProjectId:         cfg.Agent.ProjectId,
		Region:            cfg.Agent.Region,
		UserEmail:         strings.TrimSpace(email),
	}), nil
}

type liveToolCase struct {
	caseID string
	turns  []string
}

// loadLiveToolQueries accepts both probe shapes: the single-question
// {case_id, query} list the RAG probes use, and the multi-turn
// {case_id, turns:[{user}]} replay-input shape.
func loadLiveToolQueries(t *testing.T, path string) []liveToolCase {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open query list %s: %v", path, err)
	}
	defer f.Close()

	var out []liveToolCase
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1024*1024), 16*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(strings.TrimPrefix(sc.Text(), "\ufeff"))
		if line == "" {
			continue
		}
		var rec struct {
			CaseID string `json:"case_id"`
			Query  string `json:"query"`
			Turns  []struct {
				User string `json:"user"`
			} `json:"turns"`
		}
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("query list line: %v", err)
		}
		var turns []string
		if q := strings.TrimSpace(rec.Query); q != "" {
			turns = append(turns, q)
		}
		for _, tn := range rec.Turns {
			if u := strings.TrimSpace(tn.User); u != "" {
				turns = append(turns, u)
			}
		}
		if len(turns) == 0 {
			continue
		}
		out = append(out, liveToolCase{caseID: orDefault(rec.CaseID, "case-"+strconv.Itoa(len(out)+1)), turns: turns})
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan query list: %v", err)
	}
	return out
}

func dedupeStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

func truncateForLog(s string, maxRunes int) string {
	r := []rune(s)
	if len(r) <= maxRunes {
		return s
	}
	return string(r[:maxRunes]) + "…(truncated)"
}

