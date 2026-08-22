//go:build live

package sshops

import (
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/compshare-agent/internal/deployment"
	"github.com/compshare-agent/internal/opscontext"
	"github.com/compshare-agent/internal/tools"
	"github.com/compshare-agent/internal/workflow"
	"github.com/compshare-agent/internal/zones"
)

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
	params := map[string]any{
		"GpuType":             envOr("SSHH_CREATE_GPU", "4090"),
		"Gpu":                 float64(1),
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

// TestLiveOpsWriteCanary runs the real write-authorized lane but approves exactly one caller-named
// operation. It exists for reversible, dedicated-test-instance fault injection and recovery; an
// unexpected model proposal is denied. The explicit test switch plus exact command match make it impossible for the
// ordinary live suite to mutate a box accidentally.
func TestLiveOpsWriteCanary(t *testing.T) {
	if os.Getenv("SSHH_WRITE_CANARY") != "1" {
		t.Skip("set SSHH_WRITE_CANARY=1 and run this exact test")
	}
	instanceID := os.Getenv("SSHH_INSTANCE")
	task := os.Getenv("SSHH_TASK")
	approveExact := os.Getenv("SSHH_APPROVE_EXACT")
	if instanceID == "" || task == "" || approveExact == "" {
		t.Fatal("SSHH_INSTANCE, SSHH_TASK and SSHH_APPROVE_EXACT are required")
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
			if request.Command != approveExact {
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
	if !result.ContextApplied {
		t.Fatal("write canary did not deliver context to a model turn")
	}
}
