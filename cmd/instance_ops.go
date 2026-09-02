package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net"
	"strings"
	"time"

	"github.com/compshare-agent/internal/config"
	"github.com/compshare-agent/internal/engine"
	"github.com/compshare-agent/internal/governance"
	"github.com/compshare-agent/internal/opscontext"
	"github.com/compshare-agent/internal/sshops"
	"github.com/compshare-agent/internal/store"
	"github.com/compshare-agent/internal/tools"
)

// instanceOpsAction is the tool/action name the SSH-ops lane is billed under for rate limiting.
const instanceOpsAction = "DiagnoseInstanceInternals"

// instanceOpsDiagnoser is the sshops.Service surface the adapter delegates to. It is an interface
// (not the concrete *sshops.Service) so rate-limit denial can be tested without
// spawning a diagnosis.
type instanceOpsDiagnoser interface {
	DiagnoseWithContext(ctx context.Context, d sshops.Describer, owner sshops.Owner, instanceID, task string, modelContext opscontext.Context, onStep func(sshops.Step), onConfirm sshops.ConfirmFunc) (sshops.Result, error)
}

// instanceOpsRunner adapts the sshops SSH-ops core to engine.InstanceOpsRunner: it derives the tenant
// identity from the request ctx, enforces the per-tenant SSH-exec rate limit (the explicit driver call
// — R4), translates sshops.Step activity into engine.InstanceOpsProgress (synthesizing the one-shot
// "connected" event), and tallies the verdict. It holds NO per-session state, so the single shared
// instance is safe across sessions; the rate limiter isolates tenants by subject key.
type instanceOpsRunner struct {
	diag      instanceOpsDiagnoser
	describer sshops.Describer
	limiter   governance.RateLimiter // may be nil in isolated tests → no cross-turn cap
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
			// Logged for the same reason the terminal failure below is: a denial and a credential
			// failure and a spawn failure are ONE constant sentence to the user, so without a line
			// here the operator cannot tell which one happened.
			log.Printf("ssh-ops: refused instance %s: rate limited (%s)", req.InstanceID, dec.Reason)
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
		if st.AgentSessionLifecycleOnly {
			onProgress(engine.InstanceOpsProgress{
				Kind:                           engine.InstanceOpsProgressAgentSession,
				AgentSessionID:                 st.AgentSessionID,
				AgentSessionWorkdirID:          st.AgentSessionWorkdirID,
				AgentSessionContract:           st.AgentSessionContract,
				AgentSessionModel:              st.AgentSessionModel,
				AgentSessionConversationAnchor: st.AgentSessionConversationAnchor,
			})
			return
		}
		if st.JobLifecycleOnly {
			onProgress(engine.InstanceOpsProgress{
				Kind: engine.InstanceOpsProgressBackgroundJob, JobID: st.JobID, JobState: st.JobState,
				JobPurpose: st.JobPurpose,
			})
			return
		}
		if !connectedSent {
			connectedSent = true
			onProgress(engine.InstanceOpsProgress{Kind: engine.InstanceOpsProgressConnected})
		}
		// Tier: the audit row has carried it since 0014, but the live stream was dropping it, which
		// left the engine unable to tell a read from an approved write it had just watched go by.
		onProgress(engine.InstanceOpsProgress{
			Kind:        engine.InstanceOpsProgressCommand,
			Command:     st.Command,
			Tier:        st.Tier,
			Disposition: st.Disposition,
			Reason:      st.Reason,
			ExitCode:    st.ExitCode,
			Bytes:       st.Bytes,
			JobID:       st.JobID,
			JobState:    st.JobState,
			JobPurpose:  st.JobPurpose,
		})
	}

	// Entry already requires the deployment grant and a user-selected target. The private transport
	// callback grants the same scoped capability and answers older harnesses without a user card.
	onConfirm := func(sshops.ConfirmRequest) sshops.ConfirmDecision {
		return sshops.ConfirmDecision{Approved: true}
	}

	res, err := r.diag.DiagnoseWithContext(ctx, r.describer, owner, req.InstanceID, req.Task, req.Context, onStep, onConfirm)
	if err != nil {
		// Translate the no-SSH-target sentinel into the engine's transport-agnostic
		// mirror so the engine gives an honest, non-retryable refusal (e.g. a Windows
		// instance: empty SshLoginCommand) without importing internal/sshops. Every
		// other error stays the generic "couldn't complete, retry" class.
		if errors.Is(err, sshops.ErrNoSSHTarget) {
			return engine.InstanceOpsVerdict{}, engine.ErrInstanceOpsNoSSHTarget
		}
		// Not-found is knowable, non-retryable, and (on a churning account) the single
		// most likely way this lane fails. Translate it so the engine can say so.
		if errors.Is(err, sshops.ErrInstanceNotFound) {
			return engine.InstanceOpsVerdict{}, engine.ErrInstanceOpsNotFound
		}
		// A failed internal-address rewrite is a deployment-side failure, not the instance's.
		// It is logged verbatim below like every other terminal error; the sentinel exists so
		// the REPLY can say which layer failed too, which is what makes the first production
		// run of that route interpretable without server-log access.
		if errors.Is(err, sshops.ErrInternalAddressUnavailable) {
			log.Printf("ssh-ops: could not derive the internal address for instance %s: %v", req.InstanceID, err)
			return engine.InstanceOpsVerdict{}, engine.ErrInstanceOpsAddressUnavailable
		}
		// Candidate addresses existed, but none accepted the TCP connection that
		// precedes SSH authentication. Keep it separate from address derivation so
		// the reply can state exactly how far the run got without guessing whether
		// the route, firewall, SSH service or instance state was responsible.
		if errors.Is(err, sshops.ErrSSHPreflightUnreachable) {
			log.Printf("ssh-ops: no SSH candidate was reachable for instance %s: %v", req.InstanceID, err)
			return engine.InstanceOpsVerdict{}, engine.ErrInstanceOpsSSHPreflightUnreachable
		}
		// Not-running is knowable and worth naming. Carry the raw upstream state
		// through so the engine can quote it, instead of saying "please retry" about
		// a box that is, for instance, mid-image-creation.
		var notRunning *sshops.NotRunningError
		if errors.As(err, &notRunning) {
			return engine.InstanceOpsVerdict{}, fmt.Errorf("%w: %s", engine.ErrInstanceOpsNotRunning, notRunning.State)
		}
		// The user gets a stable failure sentence; the credential-free internal
		// error is logged so support can identify the failing layer.
		log.Printf("ssh-ops: diagnosis failed for instance %s: %v", req.InstanceID, err)
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
func buildSSHOpsService(sc config.SSHOpsConfig, modelFallback, apiKeyFallback string, knowledgeRetriever sshops.KnowledgeRetriever, audit sshops.AuditWriter, opts ...sshops.ServiceOption) (*sshops.Service, error) {
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
	// Both checks fail the boot instead of warning, because either way the setting would be
	// present in the config and doing nothing — and the whole reason it exists is to answer a
	// question by running. A prefix that never got used, or a typo that only surfaces as a
	// refusal on the first real diagnosis, would look like "the prefix does not work".
	if prefix := strings.TrimSpace(sc.PublicIPv6Prefix); prefix != "" {
		if !sc.InternalIPv6 {
			return nil, fmt.Errorf("agent.ssh_ops.public_ipv6_prefix needs agent.ssh_ops.internal_ipv6: the candidate list only replaces a bare-IPv4 advertised host, which is exactly what internal_ipv6 turns on")
		}
		if ip := net.ParseIP(prefix); ip == nil || ip.To4() != nil {
			return nil, fmt.Errorf("agent.ssh_ops.public_ipv6_prefix %q is not an IPv6 address", prefix)
		}
	}
	sup := sshops.Supervisor{
		Python:      sc.Python,
		HarnessPath: sc.HarnessPath,
		// Continuation is opt-in because an arbitrary OS/user temp directory may inherit a
		// CLAUDE.md from one of its ancestors. With no configured root the harness uses its
		// existing clean-workdir selection and simply starts a fresh SDK session.
		SessionRoot: strings.TrimSpace(sc.SessionRoot),
		BaseURL:     sc.BaseURL,
		APIKey:      apiKey,
		Model:       model,
		Timeout:     sc.Timeout,
		// The immutable Go retriever remains in the control-plane process. The
		// harness receives only bounded search/read replies over its sideband,
		// never the MCP URL, bearer token or search capability.
		KnowledgeRetriever: knowledgeRetriever,
	}
	// PublicIPv6Prefix rides along here rather than at the call sites for the same reason: it is
	// an addressing decision that belongs to the deployment, and threading it separately would let
	// one entrypoint wire the resolver while another forgot the prefix, producing two lanes that
	// dial differently on the same host.
	base := []sshops.ServiceOption{
		sshops.WithPublicIPv6Prefix(sc.PublicIPv6Prefix),
	}
	return sshops.NewService(sup, audit, append(base, opts...)...), nil
}

// instanceOpsHostResolver builds the internal-IPv6 address resolver, or nil to keep dialling
// the public address SshLoginCommand advertises.
//
// A misconfiguration here is an error rather than a silent downgrade: agent.ssh_ops.internal_ipv6
// is set precisely on the deployments where the public address does NOT work, so quietly falling
// back to it would produce a lane that boots, looks healthy, and times out on every instance —
// which is exactly the failure this resolver exists to end.
func instanceOpsHostResolver(cfg *config.Config, describer sshops.Describer) (sshops.HostResolver, error) {
	if !cfg.Agent.SSHOps.InternalIPv6 {
		return nil, nil
	}
	if cfg.Agent.STS.IAMURL == "" {
		return nil, fmt.Errorf("agent.ssh_ops.internal_ipv6 needs agent.sts.iam_url (the internal gateway that answers UVPCFEGO.TransformIPv4ToIPv6)")
	}
	if describer == nil {
		return nil, fmt.Errorf("agent.ssh_ops.internal_ipv6 needs an executor to resolve region ids")
	}
	return tools.NewInstanceIPv6Resolver(cfg.Agent.STS.IAMURL, describer), nil
}

// serverInstanceOpsRunner wires SSH operations only with tenant-scoped STS and
// a complete audit schema. Invalid lane configuration fails startup; a missing
// optional audit migration disables only this lane and is logged explicitly.
func serverInstanceOpsRunner(cfg *config.Config, describer sshops.Describer, knowledgeRetriever sshops.KnowledgeRetriever, db *sql.DB) (engine.InstanceOpsRunner, error) {
	if cfg == nil || strings.TrimSpace(cfg.Agent.SSHOps.HarnessPath) == "" {
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
	// Probe once at boot. Audit unavailable means no instance entry; after a
	// migration the same image must be restarted to re-enable the lane.
	probeCtx, cancelProbe := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelProbe()
	if err := store.VerifySSHOpsAuditSchema(probeCtx, db); err != nil {
		log.Printf("ssh-ops disabled: audit schema unavailable, so entering an instance could not be recorded: %v "+
			"(in-instance diagnosis stays off until the migration runs AND this process restarts — the probe is boot-only)", err)
		return nil, nil
	}
	hostResolver, err := instanceOpsHostResolver(cfg, describer)
	if err != nil {
		return nil, fmt.Errorf("ssh-ops: %w", err)
	}
	svc, err := buildSSHOpsService(
		cfg.Agent.SSHOps,
		cfg.Agent.LLM.Model,
		cfg.Agent.LLM.APIKey,
		knowledgeRetriever,
		store.NewSSHOpsAuditStore(db),
		sshops.WithHostResolver(hostResolver),
	)
	if err != nil {
		return nil, fmt.Errorf("ssh-ops: %w", err)
	}
	limiter := governance.NewInMemoryRateLimiter(cfg.Agent.RateLimit.Limits())
	route := "public address from SshLoginCommand"
	if hostResolver != nil {
		route = "internal IPv6 via " + cfg.Agent.STS.IAMURL
	}
	log.Printf("ssh-ops enabled: deployment-authorized in-instance diagnosis and recoverable repair for user-selected targets (per-tenant STS, fail-closed audit, dialling the %s; same-instance SDK/job cursors support bounded continuation)", route)
	return newInstanceOpsRunner(svc, describer, limiter), nil
}
