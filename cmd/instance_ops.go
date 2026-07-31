package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"

	"github.com/compshare-agent/internal/config"
	"github.com/compshare-agent/internal/engine"
	"github.com/compshare-agent/internal/governance"
	"github.com/compshare-agent/internal/sshops"
	"github.com/compshare-agent/internal/store"
	"github.com/compshare-agent/internal/tools"
)

// instanceOpsAction is the tool/action name the SSH-ops lane is billed under for rate limiting.
const instanceOpsAction = "DiagnoseInstanceInternals"

// instanceOpsDiagnoser is the sshops.Service surface the adapter delegates to. It is an interface
// (not the concrete *sshops.Service) so the adapter's rate-limit gate can be unit-tested with a fake
// that records whether the diagnosis was reached at all — proving a denial spawns nothing (P2 gate 5).
type instanceOpsDiagnoser interface {
	Diagnose(ctx context.Context, d sshops.Describer, owner sshops.Owner, instanceID, task string, onStep func(sshops.Step), onConfirm sshops.ConfirmFunc) (sshops.Result, error)
}

// instanceOpsRunner adapts the sshops SSH-ops core to engine.InstanceOpsRunner: it derives the tenant
// identity from the request ctx, enforces the per-tenant SSH-exec rate limit (the explicit driver call
// — R4), translates sshops.Step activity into engine.InstanceOpsProgress (synthesizing the one-shot
// "connected" event), and tallies the verdict. It holds NO per-session state, so the single shared
// instance is safe across sessions; the rate limiter isolates tenants by subject key.
type instanceOpsRunner struct {
	diag      instanceOpsDiagnoser
	describer sshops.Describer
	limiter   governance.RateLimiter // may be nil (CLI single-user path) → no cross-turn cap
}

func newInstanceOpsRunner(diag instanceOpsDiagnoser, describer sshops.Describer, limiter governance.RateLimiter) *instanceOpsRunner {
	return &instanceOpsRunner{diag: diag, describer: describer, limiter: limiter}
}

func (r *instanceOpsRunner) Run(ctx context.Context, req engine.InstanceOpsRequest, onProgress func(engine.InstanceOpsProgress)) (engine.InstanceOpsVerdict, error) {
	u, _ := tools.UserFrom(ctx)

	// Rate-limit driver call (R4): this lane never passes through SafeToolExecutor, so the SSH-exec
	// class is consumed HERE, explicitly, BEFORE the credential fetch / harness spawn. A denial returns
	// an error so the engine surfaces its honest "couldn't complete" text and never enters the box.
	if r.limiter != nil {
		subject, _ := tools.SubjectKeyFromUser(u)
		if dec := r.limiter.Allow(governance.Request{SubjectKey: subject, Class: governance.ClassSSHExec, Action: instanceOpsAction}); !dec.Allowed {
			return engine.InstanceOpsVerdict{}, fmt.Errorf("ssh-ops rate limited (%s)", dec.Reason)
		}
	}

	owner := sshops.Owner{
		TopOrganizationID: u.TopOrganizationID,
		OrganizationID:    u.OrganizationID,
		RequestUUID:       req.TurnID, // the engine turn identity IS the request identity (F21)
		TurnID:            req.TurnID, // the INV-9 (turn_id, task_hash) dedup key
	}

	// Translate the sshops activity stream into engine progress. "connected" has no wire line of its
	// own; synthesize it exactly once, right before the first command settles. A preflight failure
	// yields no commands (only a terminal verdict), so no "connected" fires — correct, we never
	// entered the box.
	connectedSent := false
	onStep := func(st sshops.Step) {
		if !connectedSent {
			connectedSent = true
			onProgress(engine.InstanceOpsProgress{Kind: engine.InstanceOpsProgressConnected})
		}
		onProgress(engine.InstanceOpsProgress{
			Kind:        engine.InstanceOpsProgressCommand,
			Command:     st.Command,
			Disposition: st.Disposition,
			ExitCode:    st.ExitCode,
			Bytes:       st.Bytes,
		})
	}

	// Adapt the engine's command-level confirmer. Staying nil when the engine supplied none is the
	// point: sshops refuses a write-enabled run without one instead of silently denying every write,
	// which would look to the user like the model failing to fix anything.
	var onConfirm sshops.ConfirmFunc
	if req.ConfirmWrite != nil {
		onConfirm = func(c sshops.ConfirmRequest) bool { return req.ConfirmWrite(c.Command) }
	}

	res, err := r.diag.Diagnose(ctx, r.describer, owner, req.InstanceID, req.Task, onStep, onConfirm)
	if err != nil {
		// Translate the no-SSH-target sentinel into the engine's transport-agnostic
		// mirror so the engine gives an honest, non-retryable refusal (e.g. a Windows
		// instance: empty SshLoginCommand) without importing internal/sshops. Every
		// other error stays the generic "couldn't complete, retry" class.
		if errors.Is(err, sshops.ErrNoSSHTarget) {
			return engine.InstanceOpsVerdict{}, engine.ErrInstanceOpsNoSSHTarget
		}
		// Not-running is knowable and worth naming. Carry the raw upstream state
		// through so the engine can quote it, instead of saying "please retry" about
		// a box that is, for instance, mid-image-creation.
		var notRunning *sshops.NotRunningError
		if errors.As(err, &notRunning) {
			return engine.InstanceOpsVerdict{}, fmt.Errorf("%w: %s", engine.ErrInstanceOpsNotRunning, notRunning.State)
		}
		return engine.InstanceOpsVerdict{}, err
	}
	ran, refused := tallySteps(res.Steps)
	return engine.InstanceOpsVerdict{Text: res.Output, Ran: ran, Refused: refused}, nil
}

// tallySteps counts the executed vs refused commands for the terminal summary line. Failed commands
// are neither: the summary is "共执行 N 条（拒绝 M 条）", and a failure is an attempt that produced no
// outcome to count as either.
func tallySteps(steps []sshops.Step) (ran, refused int) {
	for _, st := range steps {
		switch st.Disposition {
		case "ran":
			ran++
		case "refused":
			refused++
		}
	}
	return ran, refused
}

// buildSSHOpsService constructs the sshops.Service (Supervisor + audit) from config. It validates the
// harness settings so a misconfigured lane fails at boot rather than at first use.
func buildSSHOpsService(sc config.SSHOpsConfig, modelFallback, apiKeyFallback string, audit sshops.AuditWriter) (*sshops.Service, error) {
	if sc.HarnessPath == "" {
		return nil, fmt.Errorf("agent.ssh_ops.harness_path is required")
	}
	if sc.BaseURL == "" {
		return nil, fmt.Errorf("agent.ssh_ops.base_url is required")
	}
	model := sc.Model
	if model == "" {
		model = modelFallback
	}
	apiKey := sc.APIKey
	if apiKey == "" {
		apiKey = apiKeyFallback
	}
	if apiKey == "" {
		return nil, fmt.Errorf("agent.ssh_ops.api_key is required when agent.llm.api_key is empty")
	}
	// Freeze the wording gate here rather than at each call site: this function is the one place
	// that has both the config and the knowledge that the lane is actually being built, and it runs
	// once per process before any session exists. Setting it unconditionally (not only when true)
	// matters — a CLI run after a server run in the same test binary must not inherit the other's
	// mode and quietly describe the wrong product.
	tools.SetInstanceOpsWritesEnabled(sc.AllowWrites)
	sup := sshops.Supervisor{
		Python:      sc.Python,
		HarnessPath: sc.HarnessPath,
		BaseURL:     sc.BaseURL,
		APIKey:      apiKey,
		Model:       model,
		Timeout:     sc.Timeout,
		AllowWrites: sc.AllowWrites,
	}
	// Both halves come from the same config field on purpose. AllowWrites on the Supervisor is what
	// actually authorizes the harness; WithWrites only labels the audit. Wiring them from one source
	// is what keeps "what the row says we entered under" and "what the harness was allowed to do"
	// from drifting apart.
	return sshops.NewService(sup, audit, sshops.WithWrites(sc.AllowWrites)), nil
}

// serverInstanceOpsRunner decides whether the HTTP server wires the SSH-ops lane, and builds it if so.
// It returns nil (with a logged reason) — never an error for a deliberately-off lane — unless ALL hold:
//   - COMPSHARE_SSH_OPS is enabled
//   - the credential provider is per-tenant STS, not a shared static account: under static AK/SK there
//     is no upstream tenant scoping on the target instance, so the lane must refuse (INV-12 / gate 7)
//   - a database is available for the fail-closed audit store
//
// It deliberately does NOT require durable turns. The lane is READ-ONLY, so the only thing the durable
// path adds is disconnect-survival (a detached worker ctx) and per-step replay persistence — both are
// UX robustness, not safety. On the current non-durable transport a client disconnect cancels the ctx
// and kills the (read-only) harness mid-run; Diagnose still finalizes the audit row (its Finish runs on
// a WithoutCancel ctx), so the outcome is a clean retry, not corruption or an orphaned record. The same
// chatStream driver serves the legacy SSE and the non-durable WS path with the confirm card + live
// StepEvent stream, so the lane works on production today; enabling durable later only makes the
// harness survive a disconnect. A genuine misconfiguration (lane enabled but harness settings missing)
// returns an error so boot fails loudly rather than silently disabling the lane.
func serverInstanceOpsRunner(cfg *config.Config, getenv func(string) string, describer sshops.Describer, db *sql.DB) (engine.InstanceOpsRunner, error) {
	if !serverFeatureEnabled(getenv, "COMPSHARE_SSH_OPS") {
		return nil, nil
	}
	if cfg.Agent.STS.ServiceAK == "" || cfg.Agent.STS.ServiceSK == "" {
		log.Printf("ssh-ops disabled: static AK/SK provides no per-tenant instance scoping (INV-12); configure agent.sts.service_ak/service_sk")
		return nil, nil
	}
	if db == nil {
		log.Printf("ssh-ops disabled: no database for the fail-closed audit store")
		return nil, nil
	}
	svc, err := buildSSHOpsService(
		cfg.Agent.SSHOps,
		cfg.Agent.LLM.Model,
		cfg.Agent.LLM.APIKey,
		store.NewSSHOpsAuditStore(db),
	)
	if err != nil {
		return nil, fmt.Errorf("ssh-ops: %w", err)
	}
	limiter := governance.NewInMemoryRateLimiter(cfg.Agent.RateLimit.Limits())
	durable := getenv("COMPSHARE_DURABLE_TURNS") == "1"
	// deploy/ssh_ops_harness/README.md tells the operator to confirm the lane is live by finding
	// this line, so it has to name which of the two products booted. Saying "read-only" while
	// allow_writes is on is the same defect as the consent card saying it: the one place someone
	// checks would confirm the wrong thing.
	mode := "read-only diagnosis"
	if cfg.Agent.SSHOps.AllowWrites {
		mode = "diagnosis WITH REPAIR (allow_writes=true; destructive commands still refused)"
	}
	log.Printf("ssh-ops enabled: consent-gated in-instance %s (per-tenant STS, fail-closed audit, durable=%t; on the non-durable transport a client disconnect ends the run and the user retries)", mode, durable)
	return newInstanceOpsRunner(svc, describer, limiter), nil
}

// cliInstanceOpsRunner builds the SSH-ops runner for the CLI (single-user, interactive). It uses an
// in-memory audit (there is no server database on this path) and no cross-turn rate limiter — the
// interactive confirm gate plus the engine's one-run-per-turn rule (INV-11) bound it. It returns nil
// (logged) when the lane is off or misconfigured: the CLI is best-effort and must not fail to start
// over an optional lane.
func cliInstanceOpsRunner(cfg *config.Config, getenv func(string) string) engine.InstanceOpsRunner {
	switch value := getenv("COMPSHARE_SSH_OPS"); value {
	case "1":
		// enabled — build below
	case "", "0":
		return nil
	default:
		log.Printf("warning: ignoring unknown COMPSHARE_SSH_OPS value %q", value)
		return nil
	}
	svc, err := buildSSHOpsService(
		cfg.Agent.SSHOps,
		cfg.Agent.LLM.Model,
		cfg.Agent.LLM.APIKey,
		&sshops.MemAuditWriter{},
	)
	if err != nil {
		log.Printf("warning: ssh-ops disabled: %v", err)
		return nil
	}
	return newInstanceOpsRunner(svc, tools.NewExternalExecutor(cfg.Agent), nil)
}
