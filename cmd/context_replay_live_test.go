package main

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/compshare-agent/eval/replayiso"
	"github.com/compshare-agent/internal/engine"
	"github.com/compshare-agent/internal/store"
)

// Context-dependence replay harness.
//
// WHAT IT MEASURES. Whether suppressing the semantic-memory layer
// (ConversationDigest / RecentFacts / VerifiedKnowledge) changes
// the answer to a follow-up that can only be understood in context. That is the
// question step 4 of the canonical-transcript program has to answer before those
// structures are deleted.
//
// WHY IT REPLAYS ONLY USER TURNS. The July corpus predates agent_transcript_v1,
// so the stored assistant/tool rows carry no transcript. Rehydrating them and
// then turning the flag on would measure a cold-start artifact — the flag would
// have nothing to project — rather than the flag. So the harness feeds USER
// turns forward through a live engine and lets it produce its own assistant
// turns, exactly as a real session would.
//
// WHY A/A COMES FIRST. Same-binary self-comparison in this repo has measured a
// 19% flip rate. An A/B difference smaller than that floor is not a difference.
// The harness therefore runs the SAME configuration twice by default; -arm-b
// opts into the real comparison only after a floor exists.
//
// TWO STRATA, TWO VERDICTS. 35.7% of production turns called an account-state
// tool (measured over 2904 real traces). Those cannot be reproduced — the
// instance is gone, so the tool fails and the agent improvises, and answer
// differences are dominated by that rather than by the flag. Those sessions are
// judged on TOOL TRAJECTORY, not on answer text. The other 64% are
// knowledge-dominant and are judged on the answer.
//
// KNOWN LIMIT, structural. Isolation clears mcp_url (it must: a ClusterIP is not
// routable outside the cluster), so retrieval runs against the bundled corpus,
// NOT the production remote KB. Relative A/B differences remain meaningful; the
// absolute numbers do not describe production. Every report says so.

var (
	contextReplay       = flag.Bool("context-replay", false, "run the context-dependence replay (real model calls)")
	contextReplaySet    = flag.String("context-replay-set", "", "path to replay_set_context_dependence.jsonl (never committed: real user text)")
	contextReplayOut    = flag.String("context-replay-out", "", "path to write per-turn results (never under the repo)")
	contextReplayLimit  = flag.Int("context-replay-limit", 30, "max sessions to replay")
	contextReplayStrat  = flag.String("context-replay-stratum", "both", "knowledge | account | both")
	contextReplayArmB   = flag.Bool("context-replay-arm-b", false, "flip the canonical transcript in arm B (A/B). Default off = A/A noise floor")
	contextReplayConfig = flag.String("context-replay-config", "", "deploy baseline to run against")
	contextReplayTopOrg = flag.Uint64("context-replay-top-org", 0, "TEST account top_organization_id (required)")
	contextReplayOrg    = flag.Uint64("context-replay-org", 0, "TEST account organization_id (required)")
	contextReplayProj   = flag.String("context-replay-project", "", "TEST account project id")
	contextReplayEmail  = flag.String("context-replay-email", "", "user email forwarded to the platform")
	contextReplayDSN    = flag.String("context-replay-dsn", "", "NON-PRODUCTION database for the ssh-ops fail-closed audit; without it the lane cannot start and the replay diverges from production")
)

// denyEveryConfirmation is the write gate for a replay.
//
// It is not a substitute for the tenant being a test account; it is the second
// thing that has to fail. Production confirmations were human decisions a replay
// cannot reproduce, so letting the agent proceed would both create real
// resources and add variance that has nothing to do with the flag under test.
//
// The request is still recorded, because "the agent proposed a write" IS part of
// the trajectory being compared, even when the answer is no.
func denyEveryConfirmation(asked *[]string) engine.ConfirmFunc {
	return func(action string, _ map[string]any) bool {
		// Only the action name is kept. Args carry user-supplied values from real
		// sessions and this record is written to disk.
		*asked = append(*asked, strings.TrimSpace(action))
		return false
	}
}

type replayCase struct {
	SessionID      string         `json:"session_id"`
	UserTurns      []string       `json:"user_turns"`
	TurnCount      int            `json:"turn_count"`
	FollowupShapes map[string]int `json:"followup_shapes"`
}

type replayTurn struct {
	SessionID string   `json:"session_id"`
	TurnIndex int      `json:"turn_index"`
	Arm       string   `json:"arm"`
	Reply     string   `json:"reply"`
	Tools     []string `json:"tools"`
	// ConfirmAsked records write proposals the agent made. The replay always
	// denies them, but whether it ASKED is a trajectory difference worth seeing.
	ConfirmAsked []string `json:"confirm_asked,omitempty"`
	Err       string   `json:"err,omitempty"`
	Millis    int64    `json:"millis"`
}

// accountStateTool marks a tool whose result depends on live account state that
// a replay cannot reproduce. Derived from the production trace survey, not
// guessed: these are the actions that dominated the 35.7% account-dependent
// turns.
// transportFailure reports whether an error is the network failing rather than
// the product answering.
//
// This distinction is the whole reason the first run had to be thrown away. A
// mid-run outage produced 31 EOF errors on the model endpoint, and because the
// harness recorded them as ordinary turns and moved on, three sessions ended up
// with the two arms failing DIFFERENT numbers of turns — 2/9 against 9/9 in one
// case. An A/A pass over that data would score those as enormous flips and
// inflate the noise floor, which then makes a later A/B look artificially quiet.
// That error points the wrong way: it hides real differences rather than
// exaggerating them.
func transportFailure(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	for _, marker := range []string{
		"eof", "connection reset", "connection refused", "no such host",
		"i/o timeout", "tls handshake", "unexpected eof", "broken pipe",
		"context deadline exceeded", "server misbehaving", "network is unreachable",
	} {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}

// errSessionUnusable means a session could not be replayed cleanly and must be
// dropped whole. A half-failed session is worse than a missing one: it looks
// like data.
var errSessionUnusable = errors.New("session unusable: transport failed after retries")

func accountStateTool(action string) bool {
	for _, marker := range []string{"Instance", "Disk", "Monitor", "Snapshot", "Image", "Order", "Price", "Balance", "Bill"} {
		if strings.Contains(action, marker) {
			return true
		}
	}
	return false
}

func loadReplaySet(t *testing.T, path string) []replayCase {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("open replay set: %v", err)
	}
	defer func() { _ = file.Close() }()

	var cases []replayCase
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 1<<20), 1<<22)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var one replayCase
		if err := json.Unmarshal([]byte(line), &one); err != nil {
			t.Fatalf("parse replay set: %v", err)
		}
		cases = append(cases, one)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("read replay set: %v", err)
	}
	return cases
}

// replaySession feeds one session's user turns forward through a fresh engine
// and records the reply plus the tool trajectory for each turn.
func replaySession(t *testing.T, deps *engine.SharedDeps, tenantCtx context.Context, mutating bool, one replayCase, arm string) ([]replayTurn, error) {
	t.Helper()
	var asked []string
	eng := engine.NewSession(deps, engine.SessionOptions{
		// Production ships mutating_tools ON, and the flag changes the SYSTEM
		// PROMPT (segment_readonly.go drops the entire read-only boundary section
		// when writes are enabled), not just the tool window. Forcing it off would
		// make every turn — including the ~64% that never touch a write tool —
		// run under a prompt production does not use. Writes are stopped at the
		// confirmation gate instead, which is also how 联调 runs.
		MutatingToolsEnabled: mutating,
		ConfirmFn:            denyEveryConfirmation(&asked),
	})

	results := make([]replayTurn, 0, len(one.UserTurns))
	for index, userMsg := range one.UserTurns {
		var tools []string
		start := time.Now()
		askedBefore := len(asked)

		// Retry ONLY transport failures, and only a few times. A product error is
		// data; a dropped connection is not, and recording it as data is what
		// poisoned the first run.
		var reply string
		var err error
		for attempt := 0; attempt < 3; attempt++ {
			if attempt > 0 {
				time.Sleep(time.Duration(attempt*attempt) * 5 * time.Second)
				tools = nil
			}
			ctx, cancel := context.WithTimeout(tenantCtx, 3*time.Minute)
			reply, err = eng.ChatWithOptions(ctx, userMsg, func(ev engine.StepEvent) {
				if ev.Type == engine.StepToolCall && strings.TrimSpace(ev.Action) != "" {
					tools = append(tools, ev.Action)
				}
			}, engine.ChatOptions{})
			cancel()
			if !transportFailure(err) {
				break
			}
			t.Logf("  transport failure on %s turn %d attempt %d: %v", one.SessionID[:8], index, attempt+1, err)
		}
		if transportFailure(err) {
			// Drop the session whole. Its OTHER arm may have succeeded, and a pair
			// where one arm ran further than the other is not a comparison — it is
			// a flip manufactured by the network.
			return nil, fmt.Errorf("%w: %s turn %d: %v", errSessionUnusable, one.SessionID[:8], index, err)
		}

		turn := replayTurn{
			SessionID: one.SessionID,
			TurnIndex: index,
			Arm:       arm,
			Reply:     strings.TrimSpace(reply),
			Tools:     tools,
			Millis:    time.Since(start).Milliseconds(),
		}
		if len(asked) > askedBefore {
			turn.ConfirmAsked = append([]string(nil), asked[askedBefore:]...)
		}
		if err != nil {
			turn.Err = err.Error()
		}
		results = append(results, turn)
	}
	return results, nil
}

func TestContextDependenceReplay(t *testing.T) {
	if !*contextReplay {
		t.Skip("set -context-replay to run (real model calls against a live LLM)")
	}
	if *contextReplaySet == "" {
		t.Fatal("-context-replay-set is required; the replay set carries real user text and is never committed")
	}
	if *contextReplayOut == "" {
		t.Fatal("-context-replay-out is required; results carry real user text and must not land under the repo")
	}

	root := behavioralRepoRoot(t)
	if orig, err := os.Getwd(); err == nil {
		if err := os.Chdir(root); err != nil {
			t.Fatalf("chdir repo root: %v", err)
		}
		t.Cleanup(func() { _ = os.Chdir(orig) })
	}

	// The output path must not be inside the repo. eval/reports/ is gitignored on
	// main but was NOT on the stale branch this worktree sat on for weeks, and a
	// public repo plus real user text is not a combination to leave to a
	// gitignore rule being present.
	absOut, err := filepath.Abs(*contextReplayOut)
	if err != nil {
		t.Fatalf("resolve out path: %v", err)
	}
	if rel, err := filepath.Rel(root, absOut); err == nil && !strings.HasPrefix(rel, "..") {
		t.Fatalf("refusing to write replay output inside the repo (%s): it contains verbatim user text", absOut)
	}

	baseline := *contextReplayConfig
	if baseline == "" {
		baseline = filepath.Join(root, "deploy", "conf", "config.local.yaml")
	}
	// The tenant is the isolation dimension, and it is mandatory. An earlier
	// version of this harness tried to isolate by stripping credentials instead;
	// that bought nothing (which tenant a service key reaches is decided per
	// request by these IDs) and made every platform read fail, so the agent
	// reported an auth failure as a product outcome in 12 of 12 tool-calling
	// turns. Naming the account out loud is the guard.
	if *contextReplayTopOrg == 0 || *contextReplayOrg == 0 {
		t.Fatal("-context-replay-top-org and -context-replay-org are required: this replay runs " +
			"production's tool surface, so the account it may touch must be named explicitly")
	}
	isolated, err := replayiso.LoadIsolatedReplayConfig(baseline, replayiso.Tenant{
		TopOrganizationID: uint32(*contextReplayTopOrg),
		OrganizationID:    uint32(*contextReplayOrg),
		ProjectID:         *contextReplayProj,
		UserEmail:         *contextReplayEmail,
	}, *contextReplayDSN)
	if err != nil {
		t.Fatalf("isolate config: %v", err)
	}
	cfg := isolated.Config
	t.Logf("tenant: top_org=%d org=%d project=%q", isolated.Tenant.TopOrganizationID,
		isolated.Tenant.OrganizationID, cfg.Agent.ProjectId)
	t.Logf("stripped: %v", isolated.Stripped)
	// Print the inherited runtime, not just the divergence. A report that says
	// "we measured production" without naming the flags it actually ran under is
	// the kind of claim this program keeps having to retract.
	for name, value := range isolated.Inherited {
		t.Logf("inherited from deploy baseline: %s=%v", name, value)
	}

	getenv := cfg.RuntimeGetenv(os.Getenv)

	// The ssh-ops lane needs a database for its fail-closed audit and refuses to
	// start without one, so leaving the DB nil would silently drop a lane that
	// production ships ON — a divergence dressed as a safety choice. Supplying a
	// NON-PRODUCTION database keeps the lane at its production setting; isolation
	// has already refused the case where that DSN is the production one.
	var db *sql.DB
	if dsn := strings.TrimSpace(cfg.Agent.MySQL.DSN); dsn != "" {
		db, err = store.OpenMySQL(cfg.Agent.MySQL)
		if err != nil {
			t.Fatalf("open replay database: %v", err)
		}
		defer func() { _ = db.Close() }()
		if err := db.Ping(); err != nil {
			t.Fatalf("ping replay database: %v", err)
		}
		t.Log("ssh-ops lane: replay database attached (audit writes land there, not in production)")
	} else {
		t.Log("KNOWN DIVERGENCE: no replay database, so the ssh-ops lane cannot start " +
			"(serverInstanceOpsRunner needs one for its fail-closed audit) while production ships it ON. " +
			"Turns whose real resolution went through the in-instance lane are not reproduced.")
	}

	deps, mutating, err := configureSharedDepsFromEnv(cfg, getenv, db)
	if err != nil {
		t.Fatalf("shared deps: %v", err)
	}
	if want := isolated.Inherited["features.mutating_tools"]; mutating != want {
		t.Fatalf("mutating tools resolved to %v but the deploy baseline ships %v; the replay would run "+
			"under a different system prompt than production", mutating, want)
	}
	tenantCtx, err := liveProbeUserContext(cfg, uint32(*contextReplayTopOrg), uint32(*contextReplayOrg), *contextReplayEmail)
	if err != nil {
		t.Fatalf("build tenant context: %v", err)
	}

	cases := loadReplaySet(t, *contextReplaySet)
	sort.Slice(cases, func(i, j int) bool { return cases[i].TurnCount > cases[j].TurnCount })
	if len(cases) > *contextReplayLimit {
		cases = cases[:*contextReplayLimit]
	}

	out, err := os.Create(absOut)
	if err != nil {
		t.Fatalf("create out: %v", err)
	}
	defer func() { _ = out.Close() }()
	encoder := json.NewEncoder(out)

	armBLabel := "A2"
	if *contextReplayArmB {
		armBLabel = "B"
	}
	t.Logf("replaying %d sessions, arms A1/%s (%s)", len(cases), armBLabel,
		map[bool]string{true: "A/B", false: "A/A noise floor"}[*contextReplayArmB])

	var discarded []string
	consecutiveDiscards := 0
	for caseIndex, one := range cases {
		engine.SetCanonicalTranscriptEnabled(true)
		armA, errA := replaySession(t, deps, tenantCtx, mutating, one, "A1")

		var armB []replayTurn
		var errB error
		if errA == nil {
			// Arm B differs ONLY by the flag, and only when -context-replay-arm-b
			// is set. Everything else — deps, corpus, model, order — is identical,
			// which is what makes an A/A run a measurement of the harness rather
			// than of the product.
			engine.SetCanonicalTranscriptEnabled(!*contextReplayArmB)
			armB, errB = replaySession(t, deps, tenantCtx, mutating, one, armBLabel)
		}

		if errA != nil || errB != nil {
			// Neither arm is written. Keeping the arm that happened to survive is
			// exactly how the first run manufactured flips: one session recorded
			// 2/9 failed turns in A1 against 9/9 in A2, which any comparison would
			// score as a difference the flag did not cause.
			reason := errA
			if reason == nil {
				reason = errB
			}
			discarded = append(discarded, one.SessionID)
			consecutiveDiscards++
			t.Logf("[%d/%d] %s DISCARDED (both arms): %v", caseIndex+1, len(cases), one.SessionID[:8], reason)
			if consecutiveDiscards >= 3 {
				t.Fatalf("aborting: %d sessions discarded in a row. The endpoint is down, and continuing "+
					"would spend tokens producing nothing measurable", consecutiveDiscards)
			}
			continue
		}
		consecutiveDiscards = 0

		for _, turn := range append(armA, armB...) {
			if err := encoder.Encode(turn); err != nil {
				t.Fatalf("write result: %v", err)
			}
		}
		t.Logf("[%d/%d] %s turns=%d", caseIndex+1, len(cases), one.SessionID[:8], one.TurnCount)
	}

	// Coverage is reported, never implied. A run that silently dropped a third of
	// its sessions and then reported a noise floor would be describing whatever
	// survived, not the sample that was chosen.
	t.Logf("COVERAGE: %d/%d sessions replayed, %d discarded", len(cases)-len(discarded), len(cases), len(discarded))
	if len(discarded) > 0 {
		t.Logf("discarded sessions (transport failures, not product behavior): %v", discarded)
	}
	fmt.Printf("wrote %s (%d/%d sessions)\n", absOut, len(cases)-len(discarded), len(cases))
}
