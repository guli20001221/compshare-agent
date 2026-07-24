package main

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"testing"

	"github.com/compshare-agent/internal/config"
	"github.com/compshare-agent/internal/engine"
	"github.com/compshare-agent/internal/governance"
	"github.com/compshare-agent/internal/sshops"
	"github.com/compshare-agent/internal/tools"
	"github.com/stretchr/testify/require"
)

// noopDescriber is a stand-in sshops.Describer; the fake diagnoser never consults it.
type noopDescriber struct{}

func (noopDescriber) Execute(context.Context, string, map[string]any) (map[string]any, error) {
	return nil, nil
}

// fakeDiagnoser records whether/how Diagnose was reached and streams its configured steps, mirroring
// the real Service (streamed steps == returned Steps).
type fakeDiagnoser struct {
	calls          int
	output         string
	steps          []sshops.Step
	err            error
	lastOwner      sshops.Owner
	lastInstanceID string
	lastTask       string
}

func (f *fakeDiagnoser) Diagnose(_ context.Context, _ sshops.Describer, owner sshops.Owner, instanceID, task string, onStep func(sshops.Step)) (sshops.Result, error) {
	f.calls++
	f.lastOwner, f.lastInstanceID, f.lastTask = owner, instanceID, task
	for _, st := range f.steps {
		if onStep != nil {
			onStep(st)
		}
	}
	if f.err != nil {
		return sshops.Result{}, f.err
	}
	return sshops.Result{Output: f.output, Steps: f.steps}, nil
}

// fakeLimiter returns a fixed allow/deny and records the class it was asked about.
type fakeLimiter struct {
	allow     bool
	calls     int
	lastClass governance.Class
}

func (f *fakeLimiter) Allow(req governance.Request) governance.Decision {
	f.calls++
	f.lastClass = req.Class
	return governance.Decision{Allowed: f.allow, Class: req.Class, Reason: governance.ReasonQPSExceeded}
}

func intp(i int) *int { return &i }

func userCtx() context.Context {
	return tools.WithUser(context.Background(), tools.UserContext{TopOrganizationID: 7, OrganizationID: 8})
}

// P2 gate 5: a rate-limit denial must be enforced by the driver itself (this lane never passes through
// SafeToolExecutor). A denied turn reaches neither the credential fetch nor the harness.
func TestInstanceOpsRunner_RateLimitDeniedNeverDiagnoses(t *testing.T) {
	diag := &fakeDiagnoser{output: "should-not-run"}
	limiter := &fakeLimiter{allow: false}
	r := newInstanceOpsRunner(diag, noopDescriber{}, limiter)

	_, err := r.Run(userCtx(), engine.InstanceOpsRequest{TurnID: "t1", InstanceID: "uhost-x", Task: "check gpu"},
		func(engine.InstanceOpsProgress) {})

	require.Error(t, err, "a rate-limit denial must surface as an error")
	require.Equal(t, 0, diag.calls, "denied turn must not reach the diagnoser (no credential fetch, no harness)")
	require.Equal(t, governance.ClassSSHExec, limiter.lastClass, "must be billed under the ssh_exec class")
}

// The identity carried into the audit owner comes from the request ctx and the engine turn id, so the
// audit row is tenant-scoped and the INV-9 dedup key (turn_id) is populated.
func TestInstanceOpsRunner_CarriesTenantIdentityAndTurnID(t *testing.T) {
	diag := &fakeDiagnoser{output: "健康"}
	r := newInstanceOpsRunner(diag, noopDescriber{}, &fakeLimiter{allow: true})

	_, err := r.Run(userCtx(), engine.InstanceOpsRequest{TurnID: "turn-9", InstanceID: "uhost-x", Task: "check gpu"},
		func(engine.InstanceOpsProgress) {})

	require.NoError(t, err)
	require.Equal(t, 1, diag.calls)
	require.Equal(t, uint32(7), diag.lastOwner.TopOrganizationID)
	require.Equal(t, uint32(8), diag.lastOwner.OrganizationID)
	require.Equal(t, "turn-9", diag.lastOwner.TurnID, "the INV-9 dedup key must be the engine turn id")
	require.Equal(t, "turn-9", diag.lastOwner.RequestUUID)
	require.Equal(t, "uhost-x", diag.lastInstanceID)
	require.Equal(t, "check gpu", diag.lastTask)
}

// The activity stream: one synthesized "connected" (exactly once, before the first command) then one
// "command" per step, with disposition/exit/bytes passed through as metadata.
func TestInstanceOpsRunner_TranslatesActivityStream(t *testing.T) {
	diag := &fakeDiagnoser{
		output: "结论",
		steps: []sshops.Step{
			{Command: "nvidia-smi", Disposition: "ran", ExitCode: intp(0), Bytes: 42},
			{Command: "modprobe nvidia", Disposition: "refused"},
			{Command: "df -h", Disposition: "ran", ExitCode: intp(0), Bytes: 10},
		},
	}
	r := newInstanceOpsRunner(diag, noopDescriber{}, &fakeLimiter{allow: true})

	var got []engine.InstanceOpsProgress
	verdict, err := r.Run(userCtx(), engine.InstanceOpsRequest{TurnID: "t", InstanceID: "uhost-x", Task: "diag"},
		func(p engine.InstanceOpsProgress) { got = append(got, p) })

	require.NoError(t, err)
	require.Len(t, got, 4, "1 connected + 3 commands")
	require.Equal(t, engine.InstanceOpsProgressConnected, got[0].Kind)
	require.Equal(t, engine.InstanceOpsProgressCommand, got[1].Kind)
	require.Equal(t, "nvidia-smi", got[1].Command)
	require.Equal(t, "ran", got[1].Disposition)
	require.Equal(t, 42, got[1].Bytes)
	require.Equal(t, "refused", got[2].Disposition)
	require.Nil(t, got[2].ExitCode, "a refused command has no exit code")
	// tally feeds the terminal summary: 2 ran, 1 refused
	require.Equal(t, 2, verdict.Ran)
	require.Equal(t, 1, verdict.Refused)
	require.Equal(t, "结论", verdict.Text)
}

// A preflight failure yields no commands, so no "connected" fires — we never entered the box.
func TestInstanceOpsRunner_NoConnectedWhenNoCommands(t *testing.T) {
	diag := &fakeDiagnoser{output: "⚠ 只读诊断未能开始：无法建立 SSH 连接"}
	r := newInstanceOpsRunner(diag, noopDescriber{}, &fakeLimiter{allow: true})

	var got []engine.InstanceOpsProgress
	verdict, err := r.Run(userCtx(), engine.InstanceOpsRequest{TurnID: "t", InstanceID: "uhost-x", Task: "diag"},
		func(p engine.InstanceOpsProgress) { got = append(got, p) })

	require.NoError(t, err)
	require.Empty(t, got, "no command settled → no connected, no command events")
	require.Equal(t, 0, verdict.Ran)
	require.Equal(t, 0, verdict.Refused)
}

// A nil limiter (CLI single-user path) skips the rate check but still runs the diagnosis.
func TestInstanceOpsRunner_NilLimiterStillRuns(t *testing.T) {
	diag := &fakeDiagnoser{output: "健康"}
	r := newInstanceOpsRunner(diag, noopDescriber{}, nil)

	_, err := r.Run(userCtx(), engine.InstanceOpsRequest{TurnID: "t", InstanceID: "uhost-x", Task: "diag"},
		func(engine.InstanceOpsProgress) {})

	require.NoError(t, err)
	require.Equal(t, 1, diag.calls)
}

// The runner translates the sshops no-SSH-target sentinel into the engine's mirror so the engine can
// refuse honestly (e.g. a Windows instance: empty SshLoginCommand) without importing internal/sshops.
// The sshops sentinel must NOT leak past this adapter boundary.
func TestInstanceOpsRunner_TranslatesNoSSHTargetSentinel(t *testing.T) {
	diag := &fakeDiagnoser{err: fmt.Errorf("describe: %w", sshops.ErrNoSSHTarget)}
	r := newInstanceOpsRunner(diag, noopDescriber{}, &fakeLimiter{allow: true})

	_, err := r.Run(userCtx(), engine.InstanceOpsRequest{TurnID: "t", InstanceID: "uhost-x", Task: "diag"},
		func(engine.InstanceOpsProgress) {})

	require.ErrorIs(t, err, engine.ErrInstanceOpsNoSSHTarget, "no-SSH-target must become the engine sentinel")
	require.NotErrorIs(t, err, sshops.ErrNoSSHTarget, "the sshops sentinel must not leak past the adapter boundary")
}

// --- server gate decisions -------------------------------------------------------------------------

// baseGateCfg is a cfg where the ONLY thing preventing a wired lane is the flag under test.
func gateCfg() *config.Config {
	return &config.Config{Agent: config.AgentConfig{
		LLM: config.LLMConfig{Model: "deepseek-v4-flash"},
		STS: config.STSConfig{ServiceAK: "ak", ServiceSK: "sk"},
		SSHOps: config.SSHOpsConfig{
			HarnessPath: "/opt/harness.py",
			GatewayURL:  "http://127.0.0.1:3456",
		},
	}}
}

func gateEnv(vals map[string]string) func(string) string {
	return func(k string) string { return vals[k] }
}

// P2 gate 1: lane off by default → nil runner, no error.
func TestServerInstanceOpsRunner_OffByDefault(t *testing.T) {
	r, err := serverInstanceOpsRunner(gateCfg(), gateEnv(nil), noopDescriber{}, nil)
	require.NoError(t, err)
	require.Nil(t, r)
}

// The lane is READ-ONLY, so it does NOT require durable turns: with SSH_OPS + non-static STS + a DB it
// runs on the current (non-durable) production transport (WS/SSE via chatStream, which carries the
// confirm card + StepEvent stream). Durable only adds disconnect-survival, not safety.
func TestServerInstanceOpsRunner_RunsWithoutDurable(t *testing.T) {
	env := gateEnv(map[string]string{"COMPSHARE_SSH_OPS": "1"}) // COMPSHARE_DURABLE_TURNS deliberately unset
	db := sql.OpenDB(fakeConnector{})
	defer db.Close()

	r, err := serverInstanceOpsRunner(gateCfg(), env, noopDescriber{}, db)
	require.NoError(t, err)
	require.NotNil(t, r, "read-only lane must run on the non-durable transport (no durable requirement)")
}

// P2 gate 7 (INV-12): SSH_OPS + durable on, but a static provider (no STS service AK/SK) → refuse to
// construct. Under a shared static account there is no per-tenant scoping on the target instance.
func TestServerInstanceOpsRunner_RefusesStaticProvider(t *testing.T) {
	cfg := gateCfg()
	cfg.Agent.STS = config.STSConfig{} // empty service AK/SK ⇒ StaticCredentialProvider path
	cfg.Agent.PublicKey, cfg.Agent.PrivateKey = "pk", "sk"
	env := gateEnv(map[string]string{"COMPSHARE_SSH_OPS": "1"})

	// non-nil db so the ONLY thing gating construction is the static provider
	db := sql.OpenDB(fakeConnector{})
	defer db.Close()

	r, err := serverInstanceOpsRunner(cfg, env, noopDescriber{}, db)
	require.NoError(t, err)
	require.Nil(t, r, "static AK/SK must refuse the lane (INV-12)")
}

// Durable ON is accepted too — it simply adds disconnect-survival; the gate never depended on it.
func TestServerInstanceOpsRunner_ConstructsWithDurableOn(t *testing.T) {
	env := gateEnv(map[string]string{"COMPSHARE_SSH_OPS": "1", "COMPSHARE_DURABLE_TURNS": "1"})
	db := sql.OpenDB(fakeConnector{})
	defer db.Close()

	r, err := serverInstanceOpsRunner(gateCfg(), env, noopDescriber{}, db)
	require.NoError(t, err)
	require.NotNil(t, r)
}

// A fully-enabled lane with missing harness settings fails LOUDLY at boot, not silently.
func TestServerInstanceOpsRunner_MisconfigIsBootError(t *testing.T) {
	cfg := gateCfg()
	cfg.Agent.SSHOps.HarnessPath = "" // enabled but not configured
	env := gateEnv(map[string]string{"COMPSHARE_SSH_OPS": "1"})
	db := sql.OpenDB(fakeConnector{})
	defer db.Close()

	_, err := serverInstanceOpsRunner(cfg, env, noopDescriber{}, db)
	require.Error(t, err, "a fully-enabled but misconfigured lane must fail boot, not disable silently")
}

func TestBuildSSHOpsService_ValidatesAndDefaults(t *testing.T) {
	_, err := buildSSHOpsService(config.SSHOpsConfig{GatewayURL: "http://x"}, "m", &sshops.MemAuditWriter{})
	require.Error(t, err, "harness_path is required")
	_, err = buildSSHOpsService(config.SSHOpsConfig{HarnessPath: "/h.py"}, "m", &sshops.MemAuditWriter{})
	require.Error(t, err, "gateway_url is required")
	svc, err := buildSSHOpsService(config.SSHOpsConfig{HarnessPath: "/h.py", GatewayURL: "http://x"}, "m", &sshops.MemAuditWriter{})
	require.NoError(t, err)
	require.NotNil(t, svc)
}

// fakeConnector yields a non-nil *sql.DB that never actually connects (the gate only wraps it in the
// audit store; no query runs in these tests).
type fakeConnector struct{}

func (fakeConnector) Connect(context.Context) (driver.Conn, error) { return nil, fmt.Errorf("unused") }
func (fakeConnector) Driver() driver.Driver                        { return nil }
