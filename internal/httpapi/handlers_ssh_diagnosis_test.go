package httpapi

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/bitly/go-simplejson"
	"github.com/compshare-agent/internal/config"
	"github.com/compshare-agent/internal/sshops"
	"github.com/compshare-agent/internal/store"
	"github.com/gin-gonic/gin"
)

type fakeDiag struct {
	cands        []sshops.Candidate
	res          sshops.Result
	err          error
	diagInstance string
	diagTask     string
}

func (f *fakeDiag) ListCandidates(_ context.Context, _ sshops.Describer) ([]sshops.Candidate, error) {
	return f.cands, nil
}

func (f *fakeDiag) Diagnose(_ context.Context, _ sshops.Describer, _ sshops.Owner, instanceID, task string) (sshops.Result, error) {
	f.diagInstance = instanceID
	f.diagTask = task
	return f.res, f.err
}

type fakeDescriber struct{}

func (fakeDescriber) Execute(_ context.Context, _ string, _ map[string]any) (map[string]any, error) {
	return map[string]any{}, nil
}

func baseReq() BaseRequest {
	return BaseRequest{
		Action:      "StartInstanceSSHDiagnosis",
		RequestUUID: "req-1",
		Owner:       store.Owner{TopOrganizationID: 1, OrganizationID: 2},
	}
}

func enabledHandlers(diag sshDiagnoser) *Handlers {
	h := &Handlers{cfg: &config.Config{}}
	h.SetSSHOps(diag, fakeDescriber{}, nil)
	return h
}

func jsonBody(kv map[string]any) *simplejson.Json {
	j := simplejson.New()
	for k, v := range kv {
		j.Set(k, v)
	}
	return j
}

func TestStartSSHDiagnosis_DisabledByDefault(t *testing.T) {
	h := &Handlers{cfg: &config.Config{}} // sshOpsEnabled stays false
	_, err := h.handleStartInstanceSSHDiagnosis(&gin.Context{}, baseReq(), jsonBody(map[string]any{"InstanceId": "x", "Consent": true}))
	if err == nil || AsAPIError(err).Code != "InvalidParam" {
		t.Fatalf("disabled feature must reject, got %v", err)
	}
}

func TestStartSSHDiagnosis_ConsentRequired(t *testing.T) {
	h := enabledHandlers(&fakeDiag{})
	// InstanceId present, Consent absent -> must refuse BEFORE any credential work
	_, err := h.handleStartInstanceSSHDiagnosis(&gin.Context{}, baseReq(), jsonBody(map[string]any{"InstanceId": "uhost-1"}))
	if err == nil || AsAPIError(err).Code != "InvalidParam" {
		t.Fatalf("missing consent must reject, got %v", err)
	}
	// Consent=false explicitly -> still refused
	_, err = h.handleStartInstanceSSHDiagnosis(&gin.Context{}, baseReq(), jsonBody(map[string]any{"InstanceId": "uhost-1", "Consent": false}))
	if err == nil {
		t.Fatalf("Consent=false must reject")
	}
}

func TestStartSSHDiagnosis_MissingInstance(t *testing.T) {
	h := enabledHandlers(&fakeDiag{})
	_, err := h.handleStartInstanceSSHDiagnosis(&gin.Context{}, baseReq(), jsonBody(map[string]any{"Consent": true}))
	if err == nil || AsAPIError(err).Code != "InvalidParam" {
		t.Fatalf("missing InstanceId must reject, got %v", err)
	}
}

func TestStartSSHDiagnosis_HappyPath(t *testing.T) {
	diag := &fakeDiag{res: sshops.Result{Output: "根因：容器内 libnvidia-ml 缺失", ExitCode: 0}}
	h := enabledHandlers(diag)
	c := &gin.Context{Request: httptest.NewRequest("POST", "/", nil)}
	data, err := h.handleStartInstanceSSHDiagnosis(c, baseReq(), jsonBody(map[string]any{"InstanceId": "uhost-9", "Consent": true}))
	if err != nil {
		t.Fatalf("happy path: %v", err)
	}
	resp, ok := data.(startSSHDiagnosisResponse)
	if !ok {
		t.Fatalf("unexpected response type %T", data)
	}
	if resp.InstanceId != "uhost-9" || resp.Verdict != "根因：容器内 libnvidia-ml 缺失" {
		t.Fatalf("response wrong: %+v", resp)
	}
	if diag.diagInstance != "uhost-9" {
		t.Fatalf("diagnose called with wrong instance: %q", diag.diagInstance)
	}
}

func TestPrepareSSHDiagnosis(t *testing.T) {
	// disabled by default
	h0 := &Handlers{cfg: &config.Config{}}
	if _, err := h0.handlePrepareInstanceSSHDiagnosis(&gin.Context{}, baseReq(), simplejson.New()); err == nil {
		t.Fatalf("prepare must reject when disabled")
	}
	// enabled -> returns the picker + consent contract
	diag := &fakeDiag{cands: []sshops.Candidate{{InstanceID: "uhost-1", Name: "box", GpuType: "RTX3080Ti", GPU: 1, State: "Running"}}}
	h := enabledHandlers(diag)
	c := &gin.Context{Request: httptest.NewRequest("POST", "/", nil)}
	data, err := h.handlePrepareInstanceSSHDiagnosis(c, baseReq(), simplejson.New())
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	resp := data.(prepareSSHDiagnosisResponse)
	if !resp.ConsentRequired || resp.NextAction != "StartInstanceSSHDiagnosis" || len(resp.Candidates) != 1 {
		t.Fatalf("prepare response wrong: %+v", resp)
	}
}
