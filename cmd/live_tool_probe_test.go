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
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	liveToolProbe   = flag.Bool("live-tool-probe", false, "run the live end-to-end tool probe (real model + real CompShare API); off = skip")
	liveToolQueries = flag.String("live-tool-queries", "", "query JSONL: {case_id, query} or {case_id, turns:[{user}]} (required)")
	liveToolOut     = flag.String("live-tool-out", "", "write the transcript JSONL here (replay-output shape); empty = log only")
	liveToolConfig  = flag.String("live-tool-config", "", "config path; default deploy/conf/config.local.yaml")
	liveToolTopOrg  = flag.Uint("live-tool-top-org", 0, "top_organization_id for the STS role URN (required)")
	liveToolOrg     = flag.Uint("live-tool-org", 0, "organization_id (required)")
	liveToolEmail   = flag.String("live-tool-email", "", "user_email injected the way the gateway injects it (only some actions need it)")
	liveToolTimeout = flag.Duration("live-tool-timeout", 300*time.Second, "per-turn engine timeout")
	liveToolCases   = flag.Int("live-tool-cases", 0, "limit to the first N cases in file order; 0 = all")
	// One arm per run, selected here rather than by editing engine source between
	// runs — an earlier A/B did the latter and a mid-run quota kill left the two
	// arms unequally sampled with no record of which build produced which file.
	// Encode the arm in -live-tool-out; nothing in the record does.
	// Tri-state on purpose. This was a bool defaulting false, which could only
	// ever set the flag ON: deploy/conf/config.local.yaml shipped
	// forced_knowledge_hop: true and configureSharedDepsFromEnv applied that
	// before this flag was read, so an A/B run through this probe produced two
	// identical arms that read as a null effect. The config no longer sets the
	// key (the hop is off everywhere since 2026-08-01), but the tri-state stays:
	// an arm must be able to name its value rather than inherit whatever the
	// config happens to say. "" keeps the config's value; "on"/"off" override it.
	liveToolForcedHop = flag.String("live-tool-forced-hop", "", `forced first-hop retrieval arm: "on" | "off" | "" (use the config value)`)
	// DescribeCompShareInstance is region-scoped (external.go stamps the request
	// Region from the user context), so a fixed config region only ever lists that
	// region's instances. Left as the config default this silently reads empty for
	// an account whose instances live elsewhere — which looks identical to "the
	// account has none". Override per run to sweep regions.
	liveToolRegion = flag.String("live-tool-region", "", "override the tenant Region (e.g. cn-bj2, cn-sh2); empty = agent.region from config")
	// Instance-owning APIs are project-scoped; config ships project_id empty (fine
	// for the demo's read-only catalog calls) so a probe that inherits it queries
	// the account default project, not the one the frontend user's instances live
	// in. Override to the real ProjectId when verifying instance-bound behavior.
	liveToolProject = flag.String("live-tool-project", "", "override the tenant ProjectId (org-XXXX); empty = agent.project_id from config")
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

	cfgPath := orDefault(*liveToolConfig, filepath.Join(root, "deploy", "conf", "config.local.yaml"))
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("config.Load(%s): %v", cfgPath, err)
	}
	if cfg.Agent.LLM.APIKey == "" {
		t.Skip("agent.llm.api_key is empty; cannot run the live probe")
	}

	getenv := cfg.RuntimeGetenv(os.Getenv)
	// nil DB: this probe exercises the read/knowledge tool surface, not the
	// in-instance SSH lane. serverInstanceOpsRunner needs a DB for its audit
	// writer and returns a nil runner (with a logged reason) without one, so the
	// lane stays off here — which is what we want, not an accident.
	deps, mutating, err := configureSharedDepsFromEnv(cfg, getenv, nil)
	if err != nil {
		t.Fatalf("configureSharedDepsFromEnv: %v", err)
	}

	if region := strings.TrimSpace(*liveToolRegion); region != "" {
		cfg.Agent.Region = region
	}
	if project := strings.TrimSpace(*liveToolProject); project != "" {
		cfg.Agent.ProjectId = project
	}
	ctx, err := liveProbeUserContext(cfg, uint32(*liveToolTopOrg), uint32(*liveToolOrg), *liveToolEmail)
	if err != nil {
		t.Fatalf("build tenant context: %v", err)
	}
	switch strings.ToLower(strings.TrimSpace(*liveToolForcedHop)) {
	case "":
		// Config decides.
	case "on", "1", "true":
		previous := engine.ForcedKnowledgeHopEnabled()
		engine.SetForcedKnowledgeHopEnabled(true)
		t.Cleanup(func() { engine.SetForcedKnowledgeHopEnabled(previous) })
	case "off", "0", "false":
		previous := engine.ForcedKnowledgeHopEnabled()
		engine.SetForcedKnowledgeHopEnabled(false)
		t.Cleanup(func() { engine.SetForcedKnowledgeHopEnabled(previous) })
	default:
		t.Fatalf("-live-tool-forced-hop=%q: want \"on\", \"off\" or empty", *liveToolForcedHop)
	}
	t.Logf("arm: forced_knowledge_hop=%v (flag=%q, config=%v)",
		engine.ForcedKnowledgeHopEnabled(), *liveToolForcedHop, cfg.Agent.Features.ForcedKnowledgeHop != nil && *cfg.Agent.Features.ForcedKnowledgeHop)
	t.Logf("wiring: model=%s mutating=%t knowledge_mcp=%s sts=%t region=%s forced_hop=%t",
		cfg.Agent.LLM.Model, mutating, getenv("COMPSHARE_KB_MCP_URL"), cfg.Agent.STS.ServiceAK != "", cfg.Agent.Region,
		engine.ForcedKnowledgeHopEnabled())

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
		hits := map[string]retrievedChunkRec{}
		observe := func(eng *engine.Engine) {
			eng.SetRetrievalTraceObserver(func(tr observability.RetrievalTrace) {
				if len(tr.CitedChunkIDs) > 0 {
					cited = append(cited, tr.CitedChunkIDs...)
				}
				if len(tr.HitItems) > 0 || tr.Hits > 0 {
					searches++
				}
				// Union across hops: a multi-hop turn emits one trace per search,
				// and the question is whether the chunk reached the agent AT ALL.
				// Kept wins over dropped if any hop kept it.
				for _, h := range tr.HitItems {
					id := strings.TrimSpace(h.ChunkID)
					if id == "" {
						continue
					}
					if prev, seen := hits[id]; seen && (prev.Kept || prev.Score >= h.Score) {
						if prev.Kept || !h.Kept {
							continue
						}
					}
					hits[id] = retrievedChunkRec{ChunkID: id, Kept: h.Kept, Score: h.Score}
				}
			})
		}
		// One rate-limit subject per case: the sample is N different users'
		// questions, not one user asking N times, and a shared subject would
		// trip user_turn_qps and return 请求过于频繁 instead of an answer.
		rec := runCaseInProcess(ctx, deps, mutating, "live-tool-probe:"+c.caseID, c.caseID, c.history, c.turns, *liveToolTimeout, observe)
		rec.CitedChunkIDs = dedupeStrings(cited)
		rec.RetrievalTraces = searches
		ids := make([]string, 0, len(hits))
		for id := range hits {
			ids = append(ids, id)
		}
		sort.Strings(ids) // deterministic order for diffing runs
		for _, id := range ids {
			rec.RetrievedChunks = append(rec.RetrievedChunks, hits[id])
		}
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
		t.Logf("\n======== [%d/%d] %s (%dms, 前文%d条) ========\n问：%s\n工具：%s\n引用：%s\n答：\n%s",
			i+1, len(cases), c.caseID, time.Since(t0).Milliseconds(), len(c.history),
			c.turns[len(c.turns)-1], strings.Join(names, ", "),
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
	// history is the production transcript preceding the question, rehydrated
	// rather than re-sent. Empty for a first-turn case.
	history []engine.HistoryMessage
	turns   []string
}

// loadLiveToolQueries accepts three shapes.
//
// {case_id, query} \u2014 the single-question list the RAG probes use.
// {case_id, turns:[{user}]} \u2014 the replay-input shape; every turn is sent live.
// {sid, messages:[{role, content}]} \u2014 a production session export.
//
// The third is the one that needs care. Its messages are a real conversation,
// so the assistant turns in it are what the user actually read; the trailing
// user message is the question under evaluation. Sending the whole thing as
// user turns would make the agent answer its own predecessor's replies, and
// dropping the assistant turns would hand the follow-up a conversation whose
// other half is missing \u2014 a question like "\u5982\u4f55\u751f\u6210\u5bc6\u94a5\uff1f" is only answerable
// against the SSH exchange that preceded it. So everything before the trailing
// user message is rehydrated as history, exactly the way the HTTP path loads a
// session out of PostgreSQL, and only that last message is asked live.
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
			SID    string `json:"sid"`
			Query  string `json:"query"`
			Turns  []struct {
				User string `json:"user"`
			} `json:"turns"`
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("query list line: %v", err)
		}
		c := liveToolCase{caseID: orDefault(orDefault(rec.CaseID, rec.SID), "case-"+strconv.Itoa(len(out)+1))}
		if q := strings.TrimSpace(rec.Query); q != "" {
			c.turns = append(c.turns, q)
		}
		for _, tn := range rec.Turns {
			if u := strings.TrimSpace(tn.User); u != "" {
				c.turns = append(c.turns, u)
			}
		}
		if len(rec.Messages) > 0 {
			// Split at the LAST user message, not at len-1: a transcript that
			// ends on an assistant reply has no question to evaluate, and
			// silently asking the second-to-last one would score a case the
			// caller never chose.
			last := -1
			for i, m := range rec.Messages {
				if m.Role == "user" && strings.TrimSpace(m.Content) != "" {
					last = i
				}
			}
			if last < 0 {
				t.Fatalf("session %s has no user message", c.caseID)
			}
			for _, m := range rec.Messages[:last] {
				content := strings.TrimSpace(m.Content)
				if content == "" {
					continue
				}
				c.history = append(c.history, engine.HistoryMessage{Role: m.Role, Content: content})
			}
			c.turns = append(c.turns, strings.TrimSpace(rec.Messages[last].Content))
		}
		if len(c.turns) == 0 {
			continue
		}
		out = append(out, c)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan query list: %v", err)
	}
	return out
}

// TestLoadLiveToolQueriesSplitsASessionAtItsLastQuestion guards the step that
// decides what every downstream score is a score OF. A production session is
// replayed by rehydrating its transcript and asking only the trailing question;
// get the split wrong and all 53 cases are silently graded on the wrong turn,
// with output that looks entirely normal.
func TestLoadLiveToolQueriesSplitsASessionAtItsLastQuestion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.jsonl")
	require.NoError(t, os.WriteFile(path, []byte(strings.Join([]string{
		`{"sid":"MT-1","messages":[{"role":"user","content":"SSH连接有没有密钥？"},{"role":"assistant","content":"支持密钥对认证。"},{"role":"user","content":"如何生成密钥？"}]}`,
		`{"sid":"MT-2","messages":[{"role":"user","content":"4090 现在有库存吗"}]}`,
		`{"case_id":"single","query":"发票要开多久"}`,
	}, "\n")), 0o600))

	cases := loadLiveToolQueries(t, path)
	require.Len(t, cases, 3)

	assert.Equal(t, "MT-1", cases[0].caseID)
	assert.Equal(t, []string{"如何生成密钥？"}, cases[0].turns,
		"only the trailing question is asked live; the earlier user turn is context, not a question to re-answer")
	require.Len(t, cases[0].history, 2)
	assert.Equal(t, "user", cases[0].history[0].Role)
	assert.Equal(t, "assistant", cases[0].history[1].Role,
		"the real reply the user read must survive into history — dropping it leaves the follow-up unanswerable")

	assert.Empty(t, cases[1].history, "a first-turn session rehydrates nothing")
	assert.Equal(t, []string{"4090 现在有库存吗"}, cases[1].turns)

	assert.Equal(t, "single", cases[2].caseID, "the flat {case_id, query} shape still loads")
	assert.Equal(t, []string{"发票要开多久"}, cases[2].turns)
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
