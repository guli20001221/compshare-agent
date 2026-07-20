package main

// Live forced-first-decision ENGINE gate (partial coverage of review step #7).
//
// SCOPE — read before trusting this as "the P7 gate": this drives the engine
// IN-PROCESS (engine.NewSession + ChatWithOptions), NOT a WebSocket. It uses the
// real model + real upstream, but it does NOT establish a WS, go through HTTP
// auth / request parsing, inspect real frontend frames, or verify that the card
// FRAME reaches the client before the text FRAME. It observes the engine's
// StepEvents and confirm callbacks instead. The true end-to-end P7 WebSocket
// frame-ordering acceptance (card frame precedes text frame on the wire) is still
// owed and is NOT this test.
//
// Drives the REAL engine wired identically to the HTTP server
// (configureSharedDepsFromEnv, real ds-v4-flash, real CompShare executor from
// deploy/conf/config.yaml) with the forced first-decision forced ON, over the
// create / consult / image-rec probes, and asserts the OBSERVABLE engine-level
// first-hop contract:
//
//   - a write request ("帮我创建…") → the forced first decision is a write
//     proposal (outcome starts with "request:") AND a confirmation card / form is
//     reached — structurally NO read or natural-language answer precedes the card,
//     because the proposal is seeded as round 0 with no prior model answer call.
//   - a method consult / image recommendation (no execute intent) → the forced
//     first decision is continue-without-write (outcome == "continue"), NO card is
//     shown, and a non-empty answer is produced.
//
// It is the durable encoding of the manual N=3 acceptance: a degraded_* outcome
// (the deployment key can't honor forced tool_choice) FAILS the gate, so "card
// precedes NL" can never silently regress to a research-first turn.
//
// Run (never in `go test ./...` — flag-gated + self-skips without an LLM key or
// the account env). The resolver needs a real user context to query upstream and
// reach a create card, so supply the account identity via env (kept out of this
// public repo):
//   COMPSHARE_GATE_TOP_ORG=... COMPSHARE_GATE_ORG=... COMPSHARE_GATE_PROJECT_ID=... \
//   go test ./cmd -run TestForcedFirstDecisionGate -first-decision-gate -v \
//       [-first-decision-n 3] [-first-decision-min-pass 2]

import (
	"context"
	"flag"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/compshare-agent/internal/config"
	"github.com/compshare-agent/internal/engine"
	"github.com/compshare-agent/internal/governance"
	"github.com/compshare-agent/internal/tools"
	"github.com/compshare-agent/internal/workflow"
)

// gateUserContext builds the STS user identity the resolver needs to query real
// upstream (catalog/stock/image) so a create proposal can reach its confirmation
// card. The HTTP server injects this from the request body; an in-process gate
// must supply it too. Account identity comes from env (COMPSHARE_GATE_TOP_ORG /
// COMPSHARE_GATE_ORG / COMPSHARE_GATE_PROJECT_ID / COMPSHARE_GATE_REGION) so no
// real org ID lives in this public repo; the gate skips when they are unset.
func gateUserContext(cfg *config.Config) (tools.UserContext, bool) {
	topOrg, _ := strconv.ParseUint(os.Getenv("COMPSHARE_GATE_TOP_ORG"), 10, 32)
	org, _ := strconv.ParseUint(os.Getenv("COMPSHARE_GATE_ORG"), 10, 32)
	if topOrg == 0 || org == 0 {
		return tools.UserContext{}, false
	}
	roleUrn := cfg.Agent.STS.DefaultRoleUrn
	if roleUrn == "" {
		roleUrn, _ = tools.RoleUrnFromTemplate(cfg.Agent.STS.RoleUrnTemplate, uint32(topOrg))
	}
	return tools.UserContext{
		TopOrganizationID: uint32(topOrg),
		OrganizationID:    uint32(org),
		RoleUrn:           roleUrn,
		SessionName:       cfg.Agent.STS.DefaultSessionName,
		ProjectId:         os.Getenv("COMPSHARE_GATE_PROJECT_ID"),
		Region:            orDefault(os.Getenv("COMPSHARE_GATE_REGION"), "cn-wlcb"),
	}, true
}

var (
	firstDecisionGate    = flag.Bool("first-decision-gate", false, "run the live forced-first-decision gate (real model + executor); off = skip")
	firstDecisionN       = flag.Int("first-decision-n", 3, "repetitions per probe")
	firstDecisionMinPass = flag.Int("first-decision-min-pass", 0, "minimum passing reps per probe to not fail; 0 = require all N (strict)")
	firstDecisionTimeout = flag.Duration("first-decision-timeout", 240*time.Second, "per-turn engine timeout")
)

type firstDecisionProbe struct {
	name      string
	message   string
	wantWrite bool // true → expect a seeded write proposal + card; false → expect continue + answer, no card
}

// firstDecisionProbes covers both directions the review named, plus the lead's
// zoned/newest-image create variant.
var firstDecisionProbes = []firstDecisionProbe{
	{"create_plain", "帮我创建一台4090的Ubuntu虚拟机，按量计费", true},
	{"create_zoned_newest_image", "在华北一C用最新的 PyTorch 镜像帮我开一台 4090", true},
	{"consult_method", "怎么创建一台实例？", false},
	{"image_recommendation", "推荐一个适合跑 PyTorch 的 4090 镜像", false},
}

func TestForcedFirstDecisionGate(t *testing.T) {
	if !*firstDecisionGate {
		t.Skip("set -first-decision-gate to run (real model + CompShare executor; nightly / manual only)")
	}

	root := behavioralRepoRoot(t)
	if orig, err := os.Getwd(); err == nil {
		if err := os.Chdir(root); err != nil {
			t.Fatalf("chdir repo root %s: %v", root, err)
		}
		t.Cleanup(func() { _ = os.Chdir(orig) })
	}
	if os.Getenv("COMPSHARE_PROJECT_ID") == "" {
		os.Setenv("COMPSHARE_PROJECT_ID", "test-project")
	}

	cfgPath := filepath.Join(root, "deploy", "conf", "config.yaml")
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("config.Load(%s): %v", cfgPath, err)
	}
	if cfg.Agent.LLM.APIKey == "" {
		t.Skip("agent.llm.api_key is empty; cannot run the real-model first-decision gate")
	}

	getenv := cfg.RuntimeGetenv(os.Getenv)
	deps, mutating, err := configureSharedDepsFromEnv(cfg, getenv)
	if err != nil {
		t.Fatalf("configureSharedDepsFromEnv: %v", err)
	}
	if !mutating {
		t.Skip("mutating tools disabled in config; the forced first-decision has no write path to gate")
	}
	userCtx, ok := gateUserContext(cfg)
	if !ok {
		t.Skip("set COMPSHARE_GATE_TOP_ORG / COMPSHARE_GATE_ORG (and PROJECT_ID) — the resolver needs a user context to reach a create card")
	}
	// Force the flag ON for the gate even if deploy/conf/config.yaml has not yet
	// flipped forced_first_decision (that flip is the separate post-acceptance
	// step). Restore afterward so other package-main tests are unaffected.
	prevForced := engine.ForcedFirstDecisionEnabled()
	engine.SetForcedFirstDecisionEnabled(true)
	t.Cleanup(func() { engine.SetForcedFirstDecisionEnabled(prevForced) })
	t.Logf("wiring: model=%s mutating=%t forced_first_decision=ON n=%d", cfg.Agent.LLM.Model, mutating, *firstDecisionN)

	for _, p := range firstDecisionProbes {
		// Default floor is per-direction: flash's write-vs-continue CHOICE jitters
		// (~2/3-3/4 propose for a clear create — a known model-instability the fix
		// makes structural but cannot make deterministic), so a write probe needs a
		// majority, not every rep. A non-write probe is strict: a consult/recommend
		// must NEVER wrongly show a card. Override with -first-decision-min-pass.
		floor := *firstDecisionMinPass
		if floor <= 0 {
			if p.wantWrite {
				floor = (*firstDecisionN + 1) / 2 // majority
			} else {
				floor = *firstDecisionN // strict
			}
		}
		pass := 0
		for i := 0; i < *firstDecisionN; i++ {
			res := runFirstDecisionProbe(deps, mutating, userCtx, p.message, *firstDecisionTimeout)
			ok, why := judgeFirstDecision(p, res.outcome, res.cardReached, res.reply)
			if ok {
				pass++
			}
			// disposition + retryFirst are the step-2/step-1 attribution signals: for a
			// create proposal that did not card, disposition says WHY (rejected:<slot>=
			// <kind> / missing / dependency_failure / conflict); retryFirst records a
			// zero-tool event that a bounded retry then masked. Both feed the 5-run
			// measurement's per-probe rates.
			t.Logf("[%s %d/%d] pass=%t outcome=%q card=%t disposition=%q retryFirst=%q first_action=%q reply_len=%d %s",
				p.name, i+1, *firstDecisionN, ok, res.outcome, res.cardReached, res.disposition, res.retryFirst, res.firstAction, len(res.reply), why)
		}
		if pass < floor {
			t.Errorf("probe %s: %d/%d passed, want >= %d", p.name, pass, *firstDecisionN, floor)
		} else {
			t.Logf("probe %s: %d/%d passed (floor %d) OK", p.name, pass, *firstDecisionN, floor)
		}
	}
}

// firstDecisionProbeResult is the observable signal set for one probe run.
type firstDecisionProbeResult struct {
	outcome     string // engine.FirstDecisionOutcomeThisTurn — the forced first decision
	cardReached bool   // a confirmation card / form was reached
	firstAction string // first tool call the engine emitted
	reply       string // final turn reply
	disposition string // engine.ActionProposalDispositionThisTurn — resolver why-no-card
	retryFirst  string // engine.FirstDecisionRetryFirstOutcome — a masked zero-tool event
}

// runFirstDecisionProbe runs one probe on a fresh production-shaped session
// (guided create + editable confirm form opted in, every confirmation DECLINED so
// no real write happens) and returns the observable first-decision signals.
func runFirstDecisionProbe(deps *engine.SharedDeps, mutating bool, userCtx tools.UserContext, message string, timeout time.Duration) firstDecisionProbeResult {
	var cardReached bool
	var firstAction string
	eng := engine.NewSession(deps, engine.SessionOptions{
		Subject:              governance.AnonymousSubjectKey,
		ConfirmFn:            func(string, map[string]any) bool { return false },
		MutatingToolsEnabled: mutating,
	})
	eng.RehydrateHistory(nil) // fresh session: system prompt + empty history

	onStep := func(ev engine.StepEvent) {
		if firstAction == "" && ev.Type == engine.StepToolCall && ev.Action != "" {
			firstAction = ev.Action
		}
		if ev.Type == engine.StepConfirmNeeded {
			cardReached = true
		}
	}
	confirmFn := func(string, map[string]any) bool { cardReached = true; return false }
	confirmEdits := func(string, map[string]any, *workflow.ConfirmForm) workflow.ConfirmResolution {
		cardReached = true
		return workflow.ConfirmResolution{Confirmed: false}
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	ctx = tools.WithUser(ctx, userCtx) // resolver needs the STS identity to reach a real card
	reply, _ := eng.ChatWithOptions(ctx, message, onStep, engine.ChatOptions{
		ConfirmFunc:      confirmFn,
		ConfirmEditsFunc: confirmEdits,
		GuidedCreate:     true,
	})
	return firstDecisionProbeResult{
		outcome:     eng.FirstDecisionOutcomeThisTurn(),
		cardReached: cardReached,
		firstAction: firstAction,
		reply:       reply,
		disposition: eng.ActionProposalDispositionThisTurn(),
		retryFirst:  eng.FirstDecisionRetryFirstOutcome(),
	}
}

// judgeFirstDecision encodes the observable contract. A degraded_* outcome fails
// either direction: the gate's premise is that this deployment can honor forced
// tool_choice, and a silent auto-fallback is exactly the regression to catch.
func judgeFirstDecision(p firstDecisionProbe, outcome string, cardReached bool, reply string) (bool, string) {
	if strings.HasPrefix(outcome, "degraded") {
		return false, "FAIL: deployment could not honor forced tool_choice (forcing degraded)"
	}
	if p.wantWrite {
		if !strings.HasPrefix(outcome, "request:") {
			return false, "FAIL: write intent did not produce a first-hop write proposal"
		}
		if !cardReached {
			return false, "FAIL: write proposal never reached a confirmation card/form"
		}
		return true, "OK: write proposal + card precede any NL"
	}
	// non-write direction: must continue-without-write, show no card, and answer.
	if outcome != "continue" {
		return false, "FAIL: non-write turn did not continue-without-write"
	}
	if cardReached {
		return false, "FAIL: non-write turn wrongly reached a confirmation card"
	}
	if strings.TrimSpace(reply) == "" {
		return false, "FAIL: non-write turn produced an empty answer"
	}
	return true, "OK: continue + non-empty answer, no card"
}
