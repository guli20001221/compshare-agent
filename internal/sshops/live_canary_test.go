//go:build live

package sshops

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/compshare-agent/internal/deployment"
	"github.com/compshare-agent/internal/opscontext"
	"github.com/compshare-agent/internal/tools"
	"github.com/compshare-agent/internal/workflow"
	"github.com/compshare-agent/internal/zones"
)

// TestLiveOpsTaskScopeCanary proves the current product authorization contract against a disposable
// test instance: the caller supplies one trusted task-scope grant, the model may perform all
// guest-local recoverable work needed for that task, and no legacy @@CONFIRM round-trip may occur.
// The reasoning-blind destructive/form/control-plane refusals remain inside the harness and are not
// bypassed by this switch. It is opt-in and never selected by the ordinary live suite.
func TestLiveOpsTaskScopeCanary(t *testing.T) {
	if os.Getenv("SSHH_SCOPE_CANARY") != "1" {
		t.Skip("set SSHH_SCOPE_CANARY=1 and run this exact test against a disposable instance")
	}
	instanceID, task := os.Getenv("SSHH_INSTANCE"), os.Getenv("SSHH_TASK")
	if instanceID == "" || task == "" || os.Getenv("SSHH_HARNESS") == "" || os.Getenv("SSHH_API_KEY") == "" {
		t.Fatal("SSHH_INSTANCE/SSHH_TASK/SSHH_HARNESS/SSHH_API_KEY are required")
	}
	top, err := strconv.ParseUint(os.Getenv("SSHH_TOP_ORG"), 10, 32)
	if err != nil {
		t.Fatalf("SSHH_TOP_ORG: %v", err)
	}
	sub, err := strconv.ParseUint(os.Getenv("SSHH_ORG"), 10, 32)
	if err != nil {
		t.Fatalf("SSHH_ORG: %v", err)
	}
	describer, ctx := liveRealDescriber(t)
	audit := &MemAuditWriter{}
	supervisor := liveSupervisor()
	if root := os.Getenv("SSHH_SESSION_ROOT"); root != "" {
		supervisor.SessionRoot = root
	}
	modelContext := opscontext.Context{
		SchemaVersion:         opscontext.SchemaVersion,
		RepairScopeAuthorized: true,
		CurrentUserReport: &opscontext.UserReport{
			Text: task, Source: "live_test.user_report", ObservedAt: time.Now().UTC().Format(time.RFC3339),
			Status: opscontext.StatusReported,
		},
	}
	requestedAgentSessionID := strings.TrimSpace(os.Getenv("SSHH_AGENT_SESSION_ID"))
	if requestedAgentSessionID != "" {
		if supervisor.SessionRoot == "" {
			t.Fatal("SSHH_AGENT_SESSION_ID requires SSHH_SESSION_ROOT")
		}
		modelContext.AgentSession = &opscontext.AgentSession{
			SessionID: requestedAgentSessionID,
			Contract:  envOr("SSHH_AGENT_SESSION_CONTRACT", "sshops-agent-v1"),
			Model:     supervisor.Model,
			Resume:    os.Getenv("SSHH_AGENT_SESSION_RESUME") == "1",
		}
	}
	var compatibilityConfirms atomic.Int32
	var observedAgentSessionID string
	result, err := NewService(supervisor, audit).DiagnoseWithContext(
		ctx, describer,
		Owner{TopOrganizationID: uint32(top), OrganizationID: uint32(sub),
			RequestUUID: "live-scope-canary", TurnID: fmt.Sprintf("live-scope-%d", time.Now().UnixNano())},
		instanceID, task,
		modelContext,
		func(step Step) {
			if step.AgentSessionLifecycleOnly {
				observedAgentSessionID = step.AgentSessionID
				t.Logf("agent_session=%s contract=%s model=%s", step.AgentSessionID,
					step.AgentSessionContract, step.AgentSessionModel)
				return
			}
			t.Logf("step=%s tier=%s disposition=%s reason=%s", step.Command, step.Tier,
				step.Disposition, step.Reason)
		},
		func(ConfirmRequest) ConfirmDecision {
			compatibilityConfirms.Add(1)
			return ConfirmDecision{Approved: false, TerminalReason: "user_declined"}
		},
	)
	t.Logf("VERDICT:\n%s", result.Output)
	if err != nil {
		t.Fatalf("task-scope canary: %v", err)
	}
	if compatibilityConfirms.Load() != 0 {
		t.Fatalf("task-scope run emitted %d legacy command confirmation(s)", compatibilityConfirms.Load())
	}
	mutatingRan := 0
	for _, step := range result.Steps {
		if step.Tier == "mutating" && step.Disposition == "ran" {
			mutatingRan++
		}
	}
	if mutatingRan == 0 {
		t.Fatal("canary performed no guest mutation, so it did not prove autonomous repair")
	}
	if !result.ContextApplied {
		t.Fatal("task-scope canary did not deliver context to a model turn")
	}
	if requestedAgentSessionID != "" && observedAgentSessionID != requestedAgentSessionID {
		t.Fatalf("agent session receipt = %q, want %q", observedAgentSessionID, requestedAgentSessionID)
	}
	if marker := os.Getenv("SSHH_ASSERT_VERDICT_CONTAINS"); marker != "" && !strings.Contains(result.Output, marker) {
		t.Fatalf("verdict does not contain the requested continuation marker")
	}
}

// TestLiveCreateOpsCanary creates one explicitly requested, disposable test instance through the
// same catalog/capacity/price/confirmation workflow as production. It is never selected by the
// ordinary `-run TestLive` command: both the exact test name and SSHH_CREATE_CANARY=1 are required.
// The test logs only the resulting instance ID and step names, never upstream response bodies.
//
// Example:
//
//	SSHH_CREATE_CANARY=1 SSHH_CREATE_IMAGE=ComfyUI go test -tags live \
//	  -run '^TestLiveCreateOpsCanary$' -v -timeout 20m ./internal/sshops
func TestLiveCreateOpsCanary(t *testing.T) {
	if os.Getenv("SSHH_CREATE_CANARY") != "1" {
		t.Skip("set SSHH_CREATE_CANARY=1 and run this exact test to create a disposable instance")
	}
	describer, ctx := liveRealDescriber(t)
	top, err := strconv.ParseUint(os.Getenv("SSHH_TOP_ORG"), 10, 32)
	if err != nil {
		t.Fatalf("SSHH_TOP_ORG: %v", err)
	}
	sub, err := strconv.ParseUint(os.Getenv("SSHH_ORG"), 10, 32)
	if err != nil {
		t.Fatalf("SSHH_ORG: %v", err)
	}

	zoneRows, err := zones.FetchSupportZones(ctx, describer, uint32(top), uint32(sub))
	if err != nil {
		t.Fatalf("fetch live zone catalog: %v", err)
	}
	zoneEntries := make([]deployment.ZoneCatalogEntry, 0, len(zoneRows))
	for _, row := range zoneRows {
		zoneEntries = append(zoneEntries, deployment.ZoneCatalogEntry{
			Placement: deployment.ZonePlacement{
				Zone: row.Zone, Region: row.Region, ZoneID: row.ZoneID,
				AzGroup: row.RegionID, IsPod: row.IsPod,
			},
			DisplayName:           row.Describe,
			DisableImageSync:      row.DisableImageSync,
			UnsupportedImageTypes: append([]string(nil), row.UnsupportedImageTypes...),
		})
	}
	if len(zoneEntries) == 0 {
		t.Fatal("live zone catalog is empty")
	}
	if os.Getenv("SSHH_CREATE_RESOLVE_ONLY") == "1" {
		t.Logf("live zone catalog resolved (%d rows); create intentionally not attempted", len(zoneEntries))
		return
	}

	safe := tools.NewSafeToolExecutor(describer, tools.WithMutatingToolsEnabled(true))
	engine := workflow.NewEngine(
		safe.AsToolExecutor(tools.OriginWorkflowInternal),
		func(action string, _ map[string]any) bool {
			if action != "CreateInstanceWorkflow" {
				t.Fatalf("unexpected confirmation action %q", action)
			}
			t.Log("approved disposable canary create after live price/capacity resolution")
			return true
		},
		func(step workflow.StepEvent) {
			t.Logf("step=%s status=%s tool=%s", step.StepName, step.Status, step.Tool)
		},
	)
	imageSource := envOr("SSHH_CREATE_IMAGE_SOURCE", "platform")
	gpuCount, err := strconv.Atoi(envOr("SSHH_CREATE_GPU_COUNT", "1"))
	if err != nil || gpuCount < 1 {
		t.Fatalf("SSHH_CREATE_GPU_COUNT must be a positive integer, got %q", os.Getenv("SSHH_CREATE_GPU_COUNT"))
	}
	params := map[string]any{
		"GpuType":             envOr("SSHH_CREATE_GPU", "4090"),
		"Gpu":                 float64(gpuCount),
		"ChargeType":          envOr("SSHH_CREATE_CHARGE", "Postpay"),
		"ImageSource":         imageSource,
		"ImageName":           envOr("SSHH_CREATE_IMAGE", "ComfyUI"),
		"Name":                fmt.Sprintf("sshops-canary-%d", time.Now().UTC().Unix()),
		"top_organization_id": uint32(top),
		"organization_id":     uint32(sub),
	}
	if zone := os.Getenv("SSHH_CREATE_ZONE"); zone != "" {
		params["Zone"] = zone
	}
	result, err := engine.Run(ctx, workflow.CreateInstanceDef(), params,
		workflow.WithReferenceData(workflow.ReferenceData{
			ZoneCatalog:    deployment.NewZoneCatalogSnapshot(true, zoneEntries),
			ImageSelection: workflow.ImageSelectionUserPinned,
		}))
	if err != nil {
		t.Fatalf("create workflow error: %v", err)
	}
	if !result.Success {
		t.Fatalf("create workflow stopped at %q: %s", result.StoppedAt, result.Message)
	}
	ids, _ := result.Data["UHostIds"].([]any)
	if len(ids) == 0 {
		t.Fatalf("create succeeded without an instance id")
	}
	t.Logf("CREATED_CANARY_INSTANCE=%v", ids[0])
}

// TestLiveTerminateOpsCanary removes one caller-named disposable instance through the same
// tenant-scoped STS executor used by the server. It is the explicit cleanup companion to the create
// canary: the exact test name, switch and instance ID are all required, and no ordinary live run can
// select it accidentally. Only the ID is logged; upstream response bodies and credentials are not.
func TestLiveTerminateOpsCanary(t *testing.T) {
	if os.Getenv("SSHH_TERMINATE_CANARY") != "1" {
		t.Skip("set SSHH_TERMINATE_CANARY=1 and run this exact test to remove a disposable instance")
	}
	instanceID := strings.TrimSpace(os.Getenv("SSHH_INSTANCE"))
	if instanceID == "" || (!strings.HasPrefix(instanceID, "uhost-") && !strings.HasPrefix(instanceID, "cpod-")) {
		t.Fatal("SSHH_INSTANCE must be one explicit uhost-* or cpod-* disposable instance ID")
	}
	describer, ctx := liveRealDescriber(t)
	if _, err := describer.Execute(ctx, "TerminateCompShareInstance", map[string]any{
		"UHostId": instanceID, "ReleaseUDisk": true,
	}); err != nil {
		t.Fatalf("terminate disposable canary %s: %v", instanceID, err)
	}
	t.Logf("TERMINATED_CANARY_INSTANCE=%s", instanceID)
}

// TestLiveOpsWriteCanary runs the real write-authorized lane but approves exactly one caller-named
// operation. It exists for reversible, dedicated-test-instance fault injection and recovery; an
// unexpected model proposal is denied. SSHH_APPROVE_EXACT binds the whole displayed card, while
// SSHH_APPROVE_SHELL_EXACT binds the literal shell effect of a managed-background card when the
// model varies only its human-readable purpose. The explicit test switch plus exact effect match
// make it impossible for the ordinary live suite to mutate a box accidentally.
// SSHH_ASSERT_ABSENT optionally fails if one caller-supplied marker reaches the verdict or activity
// stream; durable audit redaction remains a store/PostgreSQL test because this canary uses memory.
func TestLiveOpsWriteCanary(t *testing.T) {
	if os.Getenv("SSHH_WRITE_CANARY") != "1" {
		t.Skip("set SSHH_WRITE_CANARY=1 and run this exact test")
	}
	instanceID := os.Getenv("SSHH_INSTANCE")
	task := os.Getenv("SSHH_TASK")
	approveExact := os.Getenv("SSHH_APPROVE_EXACT")
	approveShellExact := os.Getenv("SSHH_APPROVE_SHELL_EXACT")
	if instanceID == "" || task == "" || (approveExact == "") == (approveShellExact == "") {
		t.Fatal("SSHH_INSTANCE/SSHH_TASK plus exactly one of SSHH_APPROVE_EXACT or SSHH_APPROVE_SHELL_EXACT are required")
	}
	if os.Getenv("SSHH_HARNESS") == "" || os.Getenv("SSHH_API_KEY") == "" {
		t.Fatal("SSHH_HARNESS and SSHH_API_KEY are required")
	}
	top, err := strconv.ParseUint(os.Getenv("SSHH_TOP_ORG"), 10, 32)
	if err != nil {
		t.Fatalf("SSHH_TOP_ORG: %v", err)
	}
	sub, err := strconv.ParseUint(os.Getenv("SSHH_ORG"), 10, 32)
	if err != nil {
		t.Fatalf("SSHH_ORG: %v", err)
	}
	describer, ctx := liveRealDescriber(t)
	audit := &MemAuditWriter{}
	supervisor := liveSupervisor()
	approved := 0
	result, err := NewService(supervisor, audit).DiagnoseWithContext(
		ctx, describer,
		Owner{TopOrganizationID: uint32(top), OrganizationID: uint32(sub),
			RequestUUID: "live-write-canary", TurnID: fmt.Sprintf("live-write-%d", time.Now().UnixNano())},
		instanceID, task,
		opscontext.Context{SchemaVersion: opscontext.SchemaVersion,
			CurrentUserReport: &opscontext.UserReport{
				Text: task, Source: "live_test.user_report", ObservedAt: time.Now().UTC().Format(time.RFC3339),
				Status: opscontext.StatusReported,
			}},
		func(step Step) {
			t.Logf("step=%s tier=%s disposition=%s reason=%s", step.Command, step.Tier,
				step.Disposition, step.Reason)
		},
		func(request ConfirmRequest) ConfirmDecision {
			matches := request.Command == approveExact
			if approveShellExact != "" {
				const marker = " command="
				index := strings.LastIndex(request.Command, marker)
				matches = strings.HasPrefix(request.Command, "ssh_exec run_in_background=true purpose=") &&
					strings.Count(request.Command, marker) == 1 && index >= 0 &&
					request.Command[index+len(marker):] == approveShellExact
			}
			if !matches {
				t.Logf("DENIED_UNEXPECTED_OPERATION=%s", request.Command)
				return ConfirmDecision{Approved: false, TerminalReason: "user_declined"}
			}
			approved++
			t.Logf("APPROVED_EXACT_OPERATION=%s", request.Command)
			return ConfirmDecision{Approved: true}
		},
	)
	t.Logf("VERDICT:\n%s", result.Output)
	if err != nil {
		t.Fatalf("write canary: %v", err)
	}
	if approved != 1 {
		t.Fatalf("approved exact operation %d times, want 1", approved)
	}
	if marker := os.Getenv("SSHH_ASSERT_ABSENT"); marker != "" {
		if strings.Contains(result.Output, marker) {
			t.Fatal("sensitive marker leaked into the live canary verdict")
		}
		for _, step := range result.Steps {
			if strings.Contains(step.Command, marker) || strings.Contains(step.Reason, marker) {
				t.Fatal("sensitive marker leaked into a live canary activity step")
			}
		}
	}
	if !result.ContextApplied {
		t.Fatal("write canary did not deliver context to a model turn")
	}
}
