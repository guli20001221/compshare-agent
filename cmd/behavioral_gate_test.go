package main

// In-process behavioral gate (eval P0 阶段0 §②).
//
// Drives the REAL engine — same wiring as the HTTP server (configureSharedDepsFromEnv),
// real ds-v4-flash, real CompShare executor (creds from deploy/conf/config.yaml) — over the recorded
// multi-turn probe inputs, then evaluates the machine-checkable behavioral contract
// (eval/realism/ci_behavioral_gates_2026-06-22.jsonl). Each contract assertion checks
// an OBSERVABLE outcome (which tool/workflow was invoked, whether a confirm frame was
// reached, reply text) — never intent_router.intent / execution_path — so it is the
// regression safety net for later routing / taxonomy changes (阶段2/3) rather than a
// brittle re-test of the router internals.
//
// Why it lives in package main: the gate MUST drive an engine wired identically to
// production. That wiring (NewSharedDeps + applySharedDepsFromEnv + the runtime
// feature-flag setters) lives in cmd/ (configureSharedDepsFromEnv); a hand-rolled copy
// in eval/ would drift and the gate would test the wrong engine.
//
// confirm=false: the production HTTP path's session-level ConfirmFn is denyConfirm
// (no human over HTTP); the real confirm is injected per-turn. The gate mirrors this
// by declining every confirmation — which is exactly what must_confirm_mutating needs
// (any raw mutating execute under confirm=false = the confirm gate was bypassed).
//
// Run:
//   go test ./cmd -run TestBehavioralGate -behavioral-gate -v \
//       [-behavioral-cases N] [-behavioral-min-pass 80] [-behavioral-replay-out out.jsonl]
// Skipped by default (and in `go test ./...`) so the deterministic suite never makes
// real model calls — it self-skips unless -behavioral-gate is passed and the
// config has an LLM key.

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/compshare-agent/internal/config"
	"github.com/compshare-agent/internal/engine"
	"github.com/compshare-agent/internal/governance"
)

var (
	behavioralGate      = flag.Bool("behavioral-gate", false, "run the in-process behavioral gate (real model + executor); off = skip")
	behavioralCases     = flag.Int("behavioral-cases", 0, "limit to the first N contract cases (deterministic order); 0 = all")
	behavioralMinPass   = flag.Float64("behavioral-min-pass", 0, "fail the test if the BLOCK-gate pass rate (%) is below this; 0 = report-only (measurement)")
	behavioralInput     = flag.String("behavioral-input", "", "replay-input JSONL (case_id + turns[].user); default eval/realism/http_failure_replay_main_20260616_all.jsonl")
	behavioralContract  = flag.String("behavioral-contract", "", "contract assertions JSONL; default eval/realism/ci_behavioral_gates_2026-06-22.jsonl")
	behavioralConfig    = flag.String("behavioral-config", "", "config.yaml path; default deploy/conf/config.yaml")
	behavioralReplayOut = flag.String("behavioral-replay-out", "", "if set, write the produced replay-output JSONL here (checker-compatible; debug/parity)")
	behavioralTimeout   = flag.Duration("behavioral-timeout", 240*time.Second, "per-turn engine timeout")
	behavioralBudget    = flag.Duration("behavioral-budget", 0, "overall wall-clock budget across cases; stop launching new cases once exceeded and report partial (0 = no budget). Keep below the `go test -timeout` so the gate always reaches its report instead of being killed mid-run.")
)

// ---------- replay-output record (checker-compatible JSON keys) ----------

type replayCaseRecord struct {
	CaseID     string    `json:"case_id"`
	Turns      []turnRec `json:"turns"`
	FinalReply string    `json:"final_reply,omitempty"`
	Error      string    `json:"error,omitempty"`
	// CitedChunkIDs is the chunk_ids the answer cited. Markers are stripped
	// before display, so the reply text can never show whether an answer was
	// evidence-backed; only the retrieval trace can. Filled by callers that
	// attach a retrieval observer (the live probe); empty otherwise.
	CitedChunkIDs   []string `json:"cited_chunk_ids,omitempty"`
	RetrievalTraces int      `json:"retrieval_traces,omitempty"`
	// RetrievedChunks is everything retrieval surfaced this turn, kept or
	// floor-dropped. Cited ids alone cannot tell a retrieval failure from a
	// synthesis failure: "the answer did not use chunk X" and "chunk X never
	// reached the agent" look identical without this.
	RetrievedChunks []retrievedChunkRec `json:"retrieved_chunks,omitempty"`
}

type retrievedChunkRec struct {
	ChunkID string  `json:"chunk_id"`
	Kept    bool    `json:"kept"`
	Score   float64 `json:"score"`
}

type turnRec struct {
	Index             int          `json:"index"`
	User              string       `json:"user"`
	Reply             string       `json:"reply,omitempty"`
	ErrorCode         string       `json:"error_code,omitempty"`
	ErrorMessage      string       `json:"error_message,omitempty"`
	Steps             []stepRec    `json:"steps,omitempty"`
	Confirmations     []confirmRec `json:"confirmations,omitempty"`
	ConfirmationCount int          `json:"confirmation_count"`
}

type stepRec struct {
	Type    string `json:"type,omitempty"`
	Action  string `json:"action,omitempty"`
	Message string `json:"message,omitempty"`
	// Args / Result are additive debug fields (the Python checker reads only
	// type+action). Without them a transcript proves only THAT a tool was
	// called, not that the reply's specifics came from it — which is the whole
	// question a live tool probe exists to answer. Result is StepEvent.TraceResult,
	// the already-redacted payload the trace layer uses.
	Args   map[string]any `json:"args,omitempty"`
	Result map[string]any `json:"result,omitempty"`
}

type confirmRec struct {
	Action string `json:"action,omitempty"`
}

// ---------- contract assertion ----------

type contractAssertion struct {
	CaseID        string         `json:"case_id"`
	Gate          string         `json:"gate"`
	Kind          string         `json:"kind"`
	Params        contractParams `json:"params"`
	ReplySemantic bool           `json:"reply_semantic,omitempty"`
	Note          string         `json:"note,omitempty"`
}

type contractParams struct {
	Actions    []string `json:"actions,omitempty"`
	Substrings []string `json:"substrings,omitempty"`
}

// stepTypeWire mirrors httpapi.stepTypeString (the WS wire mapping the checker's
// recorded signal uses). Kept inline because that helper is unexported in
// internal/httpapi; the single source of truth is engine.StepType.
func stepTypeWire(t engine.StepType) string {
	switch t {
	case engine.StepToolCall:
		return "tool_call"
	case engine.StepToolResult:
		return "tool_result"
	case engine.StepConfirmNeeded:
		return "confirm_needed"
	case engine.StepBlocked:
		return "blocked"
	case engine.StepError:
		return "error"
	default:
		return "unknown"
	}
}

func TestBehavioralGate(t *testing.T) {
	if !*behavioralGate {
		t.Skip("set -behavioral-gate to run (real model + CompShare executor; nightly / workflow_dispatch only)")
	}

	root := behavioralRepoRoot(t)
	// Run from repo root so the engine's relative corpus / kb-sidecar paths
	// (deploy/kb/...) resolve exactly as they do for the server/CLI binary.
	if orig, err := os.Getwd(); err == nil {
		if err := os.Chdir(root); err != nil {
			t.Fatalf("chdir repo root %s: %v", root, err)
		}
		t.Cleanup(func() { _ = os.Chdir(orig) })
	}
	if os.Getenv("COMPSHARE_PROJECT_ID") == "" {
		os.Setenv("COMPSHARE_PROJECT_ID", "test-project")
	}

	cfgPath := *behavioralConfig
	if cfgPath == "" {
		cfgPath = filepath.Join(root, "deploy", "conf", "config.yaml")
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("config.Load(%s): %v", cfgPath, err)
	}
	if cfg.Agent.LLM.APIKey == "" {
		t.Skip("agent.llm.api_key is empty; cannot run the real-model behavioral gate")
	}

	getenv := cfg.RuntimeGetenv(os.Getenv)
	deps, mutating, err := configureSharedDepsFromEnv(cfg, getenv)
	if err != nil {
		t.Fatalf("configureSharedDepsFromEnv: %v", err)
	}
	t.Logf("wiring: model=%s mutating=%t rag_mode=%s",
		cfg.Agent.LLM.Model, mutating, getenv("RAG_RETRIEVAL_MODE"))

	contractPath := orDefault(*behavioralContract, filepath.Join(root, "eval", "realism", "ci_behavioral_gates_2026-06-22.jsonl"))
	inputPath := orDefault(*behavioralInput, filepath.Join(root, "eval", "realism", "http_failure_replay_main_20260616_all.jsonl"))
	assertions := loadContract(t, contractPath)
	inputs := loadReplayInputs(t, inputPath)

	caseIDs := orderedCaseIDs(assertions)
	if *behavioralCases > 0 && *behavioralCases < len(caseIDs) {
		caseIDs = caseIDs[:*behavioralCases]
	}
	t.Logf("contract: %d assertions over %d cases; running %d case(s)", len(assertions), len(orderedCaseIDs(assertions)), len(caseIDs))

	records := make(map[string]*replayCaseRecord, len(caseIDs))
	var ordered []*replayCaseRecord
	runStart := time.Now()
	for i, cid := range caseIDs {
		if *behavioralBudget > 0 && time.Since(runStart) > *behavioralBudget {
			// Stop launching new cases and fall through to the report so the gate
			// always emits its per-gate breakdown instead of being killed at the
			// `go test -timeout`. Unrun cases score NOCASE (excluded from the rate).
			t.Logf("budget %s exceeded after %d/%d cases — stopping; %d case(s) not run (NOCASE)",
				*behavioralBudget, i, len(caseIDs), len(caseIDs)-i)
			break
		}
		turns := inputs[cid]
		if len(turns) == 0 {
			t.Logf("[%d/%d] %s SKIP (no input turns in replay-input)", i+1, len(caseIDs), cid)
			continue
		}
		t0 := time.Now()
		rec := runCaseInProcess(context.Background(), deps, mutating, governance.AnonymousSubjectKey, cid, turns, *behavioralTimeout)
		records[cid] = rec
		ordered = append(ordered, rec)
		status := "ok"
		if rec.Error != "" {
			status = "ERR:" + rec.Error
		}
		t.Logf("[%d/%d] %s %s turns=%d ms=%d", i+1, len(caseIDs), cid, status, len(rec.Turns), time.Since(t0).Milliseconds())
	}

	if *behavioralReplayOut != "" {
		if err := writeReplayJSONL(*behavioralReplayOut, ordered); err != nil {
			t.Errorf("write replay out: %v", err)
		} else {
			t.Logf("wrote replay-output JSONL -> %s", *behavioralReplayOut)
		}
	}

	reportBehavioral(t, assertions, records)
}

// runCaseInProcess replicates the HTTP server's fresh-session setup
// (agentpool.buildEngine): NewSession + RehydrateHistory(nil), no Init(), and a
// per-turn ConfirmFunc that DECLINES (confirm=false) while recording the
// confirmation frame. onStep records every StepEvent.
//
// base carries the caller's tenant identity: with legacy AK/SK empty the
// executor is STS-only, and tools.UserContext is what STSProvider.Get needs to
// AssumeRole. The gate passes context.Background() (no identity — CompShare
// tool calls surface an auth error, which the behavioral contract tolerates
// because it asserts WHICH tool was selected, not what it returned); the live
// tool probe passes a real tenant so the calls actually reach the API.
//
// subject is the rate-limit bucket. Cases replayed back-to-back under ONE
// subject trip agent.rate_limit.user_turn_qps (2/s per tenant) and answer
// "请求过于频繁", which looks like a model failure but is our own limiter; a
// caller replaying N distinct users' questions passes N distinct subjects.
// configure runs against the fresh session before its first turn — the seam for
// attaching trace observers (variadic so the gate's call site is unchanged).
func runCaseInProcess(base context.Context, deps *engine.SharedDeps, mutating bool, subject, caseID string, userTurns []string, timeout time.Duration, configure ...func(*engine.Engine)) *replayCaseRecord {
	eng := engine.NewSession(deps, engine.SessionOptions{
		Subject:              subject,
		ConfirmFn:            func(string, map[string]any) bool { return false },
		MutatingToolsEnabled: mutating,
	})
	for _, fn := range configure {
		if fn != nil {
			fn(eng)
		}
	}
	eng.RehydrateHistory(nil) // fresh session: system prompt + empty history

	rec := &replayCaseRecord{CaseID: caseID}
	for i, user := range userTurns {
		var steps []stepRec
		var confirms []confirmRec
		onStep := func(ev engine.StepEvent) {
			steps = append(steps, stepRec{
				Type:    stepTypeWire(ev.Type),
				Action:  ev.Action,
				Message: ev.Message,
				Args:    ev.Args,
				Result:  ev.TraceResult,
			})
		}
		confirmFn := func(action string, _ map[string]any) bool {
			confirms = append(confirms, confirmRec{Action: action})
			return false // decline — confirm=false replay mode
		}
		ctx, cancel := context.WithTimeout(base, timeout)
		reply, cerr := eng.ChatWithOptions(ctx, user, onStep, engine.ChatOptions{ConfirmFunc: confirmFn})
		cancel()

		tr := turnRec{
			Index:             i + 1,
			User:              user,
			Reply:             reply,
			Steps:             steps,
			Confirmations:     confirms,
			ConfirmationCount: len(confirms),
		}
		if cerr != nil {
			tr.ErrorCode = "engine_error"
			tr.ErrorMessage = cerr.Error()
		}
		rec.Turns = append(rec.Turns, tr)
		if reply != "" {
			rec.FinalReply = reply
		}
		if cerr != nil {
			rec.Error = fmt.Sprintf("turn_%d: %v", i+1, cerr)
			break
		}
	}
	return rec
}

// ---------- contract evaluation (port of ci_behavioral_gates.py checker) ----------

const verdictPass, verdictFail, verdictSkipJudge, verdictNoCase = "PASS", "FAIL", "SKIP_JUDGE", "NOCASE"

func evaluateAssertion(a contractAssertion, rec *replayCaseRecord) string {
	if rec == nil {
		return verdictNoCase
	}
	switch a.Kind {
	case "require_step_action":
		if intersects(allStepActions(rec), a.Params.Actions) {
			return verdictPass
		}
		return verdictFail
	case "forbid_step_action":
		if intersects(allStepActions(rec), a.Params.Actions) {
			return verdictFail
		}
		return verdictPass
	case "require_confirm_action":
		if confirmReachedFor(rec, a.Params.Actions) {
			return verdictPass
		}
		return verdictFail
	case "require_any_confirm":
		if hasAnyConfirm(rec) {
			return verdictPass
		}
		return verdictFail
	case "forbid_reply_substring":
		for _, r := range allReplies(rec) {
			for _, sub := range a.Params.Substrings {
				if strings.Contains(r, sub) {
					return verdictFail
				}
			}
		}
		return verdictPass
	case "require_nonempty":
		if finalReplyNonEmpty(rec) {
			return verdictPass
		}
		return verdictFail
	case "judge_assisted":
		return verdictSkipJudge
	default:
		return verdictNoCase
	}
}

// allStepActions collects step.action where step.type ∈ {tool_call, confirm_needed}
// (matches the Python checker's all_step_actions).
func allStepActions(rec *replayCaseRecord) map[string]bool {
	acts := map[string]bool{}
	for _, tr := range rec.Turns {
		for _, st := range tr.Steps {
			if (st.Type == "tool_call" || st.Type == "confirm_needed") && st.Action != "" {
				acts[st.Action] = true
			}
		}
	}
	return acts
}

func confirmReachedFor(rec *replayCaseRecord, want []string) bool {
	wantSet := toSet(want)
	for _, tr := range rec.Turns {
		hasCN := tr.ConfirmationCount > 0
		for _, st := range tr.Steps {
			if st.Type == "confirm_needed" {
				hasCN = true
			}
		}
		if !hasCN {
			continue
		}
		ids := map[string]bool{}
		for _, c := range tr.Confirmations {
			if c.Action != "" {
				ids[c.Action] = true
			}
		}
		for _, st := range tr.Steps {
			if (st.Type == "confirm_needed" || st.Type == "tool_call") && st.Action != "" {
				ids[st.Action] = true
			}
		}
		for id := range ids {
			if wantSet[id] {
				return true
			}
		}
	}
	return false
}

func hasAnyConfirm(rec *replayCaseRecord) bool {
	for _, tr := range rec.Turns {
		if tr.ConfirmationCount > 0 {
			return true
		}
		for _, st := range tr.Steps {
			if st.Type == "confirm_needed" {
				return true
			}
		}
	}
	return false
}

func allReplies(rec *replayCaseRecord) []string {
	var out []string
	for _, tr := range rec.Turns {
		if tr.Reply != "" {
			out = append(out, tr.Reply)
		}
	}
	if rec.FinalReply != "" {
		out = append(out, rec.FinalReply)
	}
	return out
}

func finalReplyNonEmpty(rec *replayCaseRecord) bool {
	if strings.TrimSpace(rec.FinalReply) == "" {
		return false
	}
	if rec.Error != "" {
		return false
	}
	if n := len(rec.Turns); n > 0 && rec.Turns[n-1].ErrorCode != "" {
		return false
	}
	return true
}

// reportBehavioral aggregates verdicts, prints a per-gate breakdown, and (when
// -behavioral-min-pass > 0) fails if the BLOCK-gate pass rate is below the floor.
// A gate is WARN (advisory, never blocks) iff kind == judge_assisted OR reply_semantic
// — matching spec §6 (state_honest / route_no_misroute(reply_semantic) / slot_retain
// go to a judge/manual bypass). All other gates are BLOCK (machine-checkable hard gates).
func reportBehavioral(t *testing.T, assertions []contractAssertion, records map[string]*replayCaseRecord) {
	type tally struct{ pass, fail, skip, nocase int }
	overall := tally{}
	byGate := map[string]*tally{}
	var blockPass, blockTotal, warnPass, warnTotal int
	var blockFails []string

	for _, a := range assertions {
		v := evaluateAssertion(a, records[a.CaseID])
		g := byGate[a.Gate]
		if g == nil {
			g = &tally{}
			byGate[a.Gate] = g
		}
		warn := a.Kind == "judge_assisted" || a.ReplySemantic
		switch v {
		case verdictPass:
			overall.pass++
			g.pass++
			if warn {
				warnPass++
				warnTotal++
			} else {
				blockPass++
				blockTotal++
			}
		case verdictFail:
			overall.fail++
			g.fail++
			if warn {
				warnTotal++
			} else {
				blockTotal++
				blockFails = append(blockFails, fmt.Sprintf("%s/%s(%s)", a.CaseID, a.Gate, a.Kind))
			}
		case verdictSkipJudge:
			overall.skip++
			g.skip++
		case verdictNoCase:
			overall.nocase++
			g.nocase++
		}
	}

	t.Logf("OVERALL: pass=%d fail=%d skip_judge=%d nocase=%d (total=%d)",
		overall.pass, overall.fail, overall.skip, overall.nocase, len(assertions))
	gates := make([]string, 0, len(byGate))
	for g := range byGate {
		gates = append(gates, g)
	}
	sort.Strings(gates)
	for _, g := range gates {
		tl := byGate[g]
		t.Logf("  gate %-22s pass=%d fail=%d skip=%d nocase=%d", g, tl.pass, tl.fail, tl.skip, tl.nocase)
	}

	blockRate, warnRate := 0.0, 0.0
	if blockTotal > 0 {
		blockRate = 100 * float64(blockPass) / float64(blockTotal)
	}
	if warnTotal > 0 {
		warnRate = 100 * float64(warnPass) / float64(warnTotal)
	}
	t.Logf("BLOCK gates: %d/%d pass = %.1f%%   WARN gates: %d/%d pass = %.1f%%",
		blockPass, blockTotal, blockRate, warnPass, warnTotal, warnRate)
	if len(blockFails) > 0 {
		t.Logf("BLOCK failures (%d): %s", len(blockFails), strings.Join(blockFails, ", "))
	}

	if *behavioralMinPass > 0 {
		if blockRate < *behavioralMinPass {
			t.Errorf("BLOCK-gate pass rate %.1f%% < floor %.1f%%", blockRate, *behavioralMinPass)
		}
	} else {
		t.Logf("report-only (no -behavioral-min-pass floor set); not failing on pass rate")
	}
}

// ---------- loaders / helpers ----------

func behavioralRepoRoot(t *testing.T) string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	// file = <root>/cmd/behavioral_gate_test.go → up two = <root>
	return filepath.Dir(filepath.Dir(file))
}

func loadContract(t *testing.T, path string) []contractAssertion {
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open contract %s: %v", path, err)
	}
	defer f.Close()
	var out []contractAssertion
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1024*1024), 8*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var a contractAssertion
		if err := json.Unmarshal([]byte(line), &a); err != nil {
			t.Fatalf("contract line: %v", err)
		}
		out = append(out, a)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan contract: %v", err)
	}
	return out
}

// loadReplayInputs reads case_id + ordered turns[].user from the recorded replay
// JSONL (only the user inputs are used; recorded replies/steps are ignored).
func loadReplayInputs(t *testing.T, path string) map[string][]string {
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open replay-input %s: %v", path, err)
	}
	defer f.Close()
	out := map[string][]string{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1024*1024), 16*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var rec struct {
			CaseID string `json:"case_id"`
			Turns  []struct {
				User string `json:"user"`
			} `json:"turns"`
		}
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("replay-input line: %v", err)
		}
		if rec.CaseID == "" {
			continue
		}
		var turns []string
		for _, tn := range rec.Turns {
			if strings.TrimSpace(tn.User) != "" {
				turns = append(turns, tn.User)
			}
		}
		out[rec.CaseID] = turns
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan replay-input: %v", err)
	}
	return out
}

func orderedCaseIDs(assertions []contractAssertion) []string {
	seen := map[string]bool{}
	var out []string
	for _, a := range assertions {
		if !seen[a.CaseID] {
			seen[a.CaseID] = true
			out = append(out, a.CaseID)
		}
	}
	return out
}

func writeReplayJSONL(path string, records []*replayCaseRecord) error {
	if dir := filepath.Dir(path); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetEscapeHTML(false)
	for _, rec := range records {
		if err := enc.Encode(rec); err != nil {
			return err
		}
	}
	return nil
}

func intersects(set map[string]bool, want []string) bool {
	for _, w := range want {
		if set[w] {
			return true
		}
	}
	return false
}

func toSet(items []string) map[string]bool {
	s := make(map[string]bool, len(items))
	for _, it := range items {
		s[it] = true
	}
	return s
}

func orDefault(v, def string) string {
	if v != "" {
		return v
	}
	return def
}
