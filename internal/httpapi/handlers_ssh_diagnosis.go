package httpapi

import (
	"context"
	"strings"

	"github.com/bitly/go-simplejson"
	"github.com/compshare-agent/internal/governance"
	"github.com/compshare-agent/internal/sshops"
	"github.com/compshare-agent/internal/tools"
	"github.com/gin-gonic/gin"
)

// sshDiagnoser is the slice of *sshops.Service the Action handlers use, kept as an interface so
// tests can substitute a fake without spawning a real harness.
type sshDiagnoser interface {
	ListCandidates(ctx context.Context, d sshops.Describer) ([]sshops.Candidate, error)
	Diagnose(ctx context.Context, d sshops.Describer, owner sshops.Owner, instanceID, task string) (sshops.Result, error)
}

// prepareSSHDiagnosisResponse is the consent-picker payload: candidate instances + an explicit
// consent contract. The frontend renders the picker; on selection + authorization it calls
// StartInstanceSSHDiagnosis with {InstanceId, Consent:true}. This is Option A's decoupled lane —
// nothing here runs SSH; the SSH only happens in the consent-gated Start Action.
type prepareSSHDiagnosisResponse struct {
	Message         string             `json:"Message"`
	ConsentRequired bool               `json:"ConsentRequired"`
	NextAction      string             `json:"NextAction"`
	Candidates      []sshops.Candidate `json:"Candidates"`
}

type startSSHDiagnosisResponse struct {
	InstanceId string `json:"InstanceId"`
	Verdict    string `json:"Verdict"`
	ExitCode   int    `json:"ExitCode"`
	TimedOut   bool   `json:"TimedOut"`
}

// handlePrepareInstanceSSHDiagnosis lists the caller's instances for the consent picker. It runs
// no SSH and needs no consent — it only surfaces the choice + the consent requirement.
func (h *Handlers) handlePrepareInstanceSSHDiagnosis(c *gin.Context, base BaseRequest, _ *simplejson.Json) (any, error) {
	if !h.sshOpsEnabled || h.sshOps == nil {
		return nil, ErrInvalidParam.WithMessage("instance SSH diagnosis is not enabled")
	}
	uc, err := h.buildUserContext(base)
	if err != nil {
		return nil, err
	}
	ctx := tools.WithUser(c.Request.Context(), uc)
	cands, err := h.sshOps.ListCandidates(ctx, h.sshDescriber)
	if err != nil {
		return nil, ErrInternal.WithMessage("list instances: %v", err)
	}
	return prepareSSHDiagnosisResponse{
		Message:         "检测到可能的实例内问题（如掉卡 / 驱动异常）。请选择要进入排查的实例，并授权对该实例执行只读 SSH 诊断。",
		ConsentRequired: true,
		NextAction:      "StartInstanceSSHDiagnosis",
		Candidates:      cands,
	}, nil
}

// handleStartInstanceSSHDiagnosis runs ONE consented, read-only in-instance diagnosis. Consent is
// structural: without Consent==true the Action refuses before any credential is fetched.
func (h *Handlers) handleStartInstanceSSHDiagnosis(c *gin.Context, base BaseRequest, raw *simplejson.Json) (any, error) {
	if !h.sshOpsEnabled || h.sshOps == nil {
		return nil, ErrInvalidParam.WithMessage("instance SSH diagnosis is not enabled")
	}
	instanceID := raw.Get("InstanceId").MustString()
	if instanceID == "" {
		return nil, ErrInvalidParam.WithMessage("missing InstanceId")
	}
	if !consentGranted(raw) {
		return nil, ErrInvalidParam.WithMessage("Consent required: set Consent=true to authorize read-only SSH diagnosis")
	}
	// Per-tenant rate limit (opt-in; inert until ops configures ssh_exec limits).
	if h.sshLimiter != nil {
		subject := governance.SubjectKeyFromTenant(int64(base.Owner.TopOrganizationID), int64(base.Owner.OrganizationID))
		if dec := h.sshLimiter.Allow(governance.Request{SubjectKey: subject, Class: governance.ClassSSHExec, Action: "ssh_diagnose"}); !dec.Allowed {
			return nil, ErrRateLimited
		}
	}
	uc, err := h.buildUserContext(base)
	if err != nil {
		return nil, err
	}
	ctx := tools.WithUser(c.Request.Context(), uc)
	owner := sshops.Owner{
		TopOrganizationID: base.Owner.TopOrganizationID,
		OrganizationID:    base.Owner.OrganizationID,
		RequestUUID:       base.RequestUUID,
	}
	res, err := h.sshOps.Diagnose(ctx, h.sshDescriber, owner, instanceID, raw.Get("Task").MustString())
	if err != nil {
		// Credential-free by construction (sshops never serializes the secret).
		return nil, ErrInternal.WithMessage("diagnosis failed: %v", err)
	}
	return startSSHDiagnosisResponse{
		InstanceId: instanceID,
		Verdict:    res.Output,
		ExitCode:   res.ExitCode,
		TimedOut:   res.TimedOut,
	}, nil
}

// consentGranted accepts a JSON bool or a form-encoded "true"/"1"/"yes" string.
func consentGranted(raw *simplejson.Json) bool {
	if b, err := raw.Get("Consent").Bool(); err == nil {
		return b
	}
	switch strings.ToLower(strings.TrimSpace(raw.Get("Consent").MustString())) {
	case "true", "1", "yes":
		return true
	}
	return false
}
