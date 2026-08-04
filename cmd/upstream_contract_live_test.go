//go:build live

package main

// Opt-in live contract probe for the CompShare public API.
//
// This is intentionally a thin caller rather than an Agent conversation: it
// tells us whether the upstream wire contract itself accepts a request before
// the same operation is exercised through a workflow. It uses the production
// ExternalExecutor, including per-tenant STS and the public gateway.

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/compshare-agent/internal/config"
	"github.com/compshare-agent/internal/deployment"
	"github.com/compshare-agent/internal/security"
	"github.com/compshare-agent/internal/tools"
	"github.com/compshare-agent/internal/workflow"
	"github.com/compshare-agent/internal/zones"
)

var (
	upstreamContractLive       = flag.Bool("upstream-contract-live", false, "run one real CompShare API action")
	upstreamContractConfig     = flag.String("upstream-contract-config", "", "config path; default deploy/conf/config.local.yaml")
	upstreamContractTopOrg     = flag.Uint("upstream-contract-top-org", 0, "top organization id")
	upstreamContractOrg        = flag.Uint("upstream-contract-org", 0, "organization id")
	upstreamContractProject    = flag.String("upstream-contract-project", "", "project id")
	upstreamContractRegion     = flag.String("upstream-contract-region", "", "context region")
	upstreamContractEmail      = flag.String("upstream-contract-email", "", "gateway-authenticated user email")
	upstreamContractAccount    = flag.Uint("upstream-contract-account", 0, "gateway-authenticated account/user id")
	upstreamContractAction     = flag.String("upstream-contract-action", "", "upstream action")
	upstreamContractArgs       = flag.String("upstream-contract-args", "{}", "JSON object with action arguments")
	upstreamContractFlow       = flag.String("upstream-contract-workflow", "", "run a workflow instead of a raw action (create-custom-image, create-disk, create-cfs)")
	upstreamContractStopAt     = flag.String("upstream-contract-expect-stop-at", "", "expect the workflow to stop safely at this step")
	upstreamContractAllowWrite = flag.Bool("upstream-contract-allow-write", false,
		"acknowledge that the selected live workflow or L1 action may modify tenant resources")
)

func TestUpstreamContractLive(t *testing.T) {
	if !*upstreamContractLive {
		t.Skip("set -upstream-contract-live to run")
	}
	if *upstreamContractTopOrg == 0 || *upstreamContractOrg == 0 {
		t.Fatal("top org and org are required")
	}
	if strings.TrimSpace(*upstreamContractAction) == "" && strings.TrimSpace(*upstreamContractFlow) == "" {
		t.Fatal("action is required")
	}
	if err := guardUpstreamContractWrite(*upstreamContractAction, *upstreamContractFlow, *upstreamContractAllowWrite); err != nil {
		t.Fatal(err)
	}

	root := behavioralRepoRoot(t)
	cfgPath := *upstreamContractConfig
	if cfgPath == "" {
		cfgPath = filepath.Join(root, "deploy", "conf", "config.local.yaml")
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if region := strings.TrimSpace(*upstreamContractRegion); region != "" {
		cfg.Agent.Region = region
	}
	if project := strings.TrimSpace(*upstreamContractProject); project != "" {
		cfg.Agent.ProjectId = project
	}
	ctx, err := upstreamContractUserContext(cfg, uint32(*upstreamContractTopOrg), uint32(*upstreamContractOrg), uint32(*upstreamContractAccount), *upstreamContractEmail)
	if err != nil {
		t.Fatalf("build tenant context: %v", err)
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	args := map[string]any{}
	if err := json.Unmarshal([]byte(*upstreamContractArgs), &args); err != nil {
		t.Fatalf("decode args: %v", err)
	}
	executor := tools.NewExternalExecutor(cfg.Agent)
	if *upstreamContractFlow != "" {
		runUpstreamContractWorkflow(t, ctx, executor, *upstreamContractFlow, args, *upstreamContractStopAt)
		return
	}
	got, err := executor.Execute(ctx, *upstreamContractAction, args)
	if err != nil {
		t.Fatalf("%s: %v", *upstreamContractAction, err)
	}
	safe := redactLiveContractResponse(got)
	encoded, _ := json.MarshalIndent(safe, "", "  ")
	t.Logf("%s response:\n%s", *upstreamContractAction, encoded)
}

// upstreamContractUserContext deliberately mirrors httpapi.Handlers' request
// path. In particular, a configured default STS role takes precedence over the
// per-company role template; otherwise a live probe can test a different tenant
// identity from the server it is intended to validate.
func upstreamContractUserContext(cfg *config.Config, topOrg, org, account uint32, email string) (context.Context, error) {
	roleURN := strings.TrimSpace(cfg.Agent.STS.DefaultRoleUrn)
	if roleURN == "" {
		var err error
		roleURN, err = tools.RoleUrnFromTemplate(cfg.Agent.STS.RoleUrnTemplate, topOrg)
		if err != nil {
			return nil, err
		}
	}
	return tools.WithUser(context.Background(), tools.UserContext{
		TopOrganizationID: topOrg,
		OrganizationID:    org,
		CompanyID:         topOrg,
		AccountID:         account,
		RoleUrn:           roleURN,
		SessionName:       fmt.Sprintf("%d-%d", topOrg, org),
		ProjectId:         cfg.Agent.ProjectId,
		Region:            cfg.Agent.Region,
		UserEmail:         strings.TrimSpace(email),
	}), nil
}

// guardUpstreamContractWrite makes an opt-in live probe safe by default. Its
// workflow confirmation callback is intentionally automatic, so a separate CLI
// acknowledgement is required before it can issue any L1 request.
func guardUpstreamContractWrite(action, flow string, allowWrite bool) error {
	if strings.TrimSpace(flow) != "" {
		if !allowWrite {
			return fmt.Errorf("refusing live workflow %q: it may modify tenant resources; re-run with -upstream-contract-allow-write only for a disposable test target", flow)
		}
		return nil
	}
	level, err := security.Check(strings.TrimSpace(action))
	if err != nil {
		return fmt.Errorf("refusing unclassified live action %q: use a security-classified action or a typed workflow (%w)", action, err)
	}
	switch level {
	case security.L0:
		return nil
	case security.L1:
		if !allowWrite {
			return fmt.Errorf("refusing live mutating action %q: re-run with -upstream-contract-allow-write only for a disposable test target", action)
		}
		return nil
	default:
		return fmt.Errorf("refusing destructive live action %q", action)
	}
}

func TestUpstreamContractUserContextPrefersConfiguredDefaultRole(t *testing.T) {
	cfg := &config.Config{Agent: config.AgentConfig{
		Region:    "cn-sh2",
		ProjectId: "tenant-project",
		STS: config.STSConfig{
			DefaultRoleUrn:  "ucs:iam::shared:role/contract-test",
			RoleUrnTemplate: "ucs:iam::%d:role/ignored-when-default-is-set",
		},
	}}
	ctx, err := upstreamContractUserContext(cfg, 101, 202, 303, "owner@example.com")
	if err != nil {
		t.Fatalf("upstreamContractUserContext: %v", err)
	}
	user, ok := tools.UserFrom(ctx)
	if !ok {
		t.Fatal("expected tenant user context")
	}
	if user.RoleUrn != "ucs:iam::shared:role/contract-test" {
		t.Fatalf("RoleUrn = %q, want configured default", user.RoleUrn)
	}
	if user.SessionName != "101-202" {
		t.Fatalf("SessionName = %q, want HTTP request-path session name", user.SessionName)
	}
}

func TestGuardUpstreamContractWrite(t *testing.T) {
	cases := []struct {
		name       string
		action     string
		flow       string
		allowWrite bool
		wantErr    bool
	}{
		{name: "read action", action: "DescribeCompShareCustomImages"},
		{name: "mutating action needs acknowledgement", action: "CreateCompShareCustomImage", wantErr: true},
		{name: "mutating action acknowledged", action: "CreateCompShareCustomImage", allowWrite: true},
		{name: "workflow needs acknowledgement", flow: "create-custom-image", wantErr: true},
		{name: "workflow acknowledged", flow: "create-custom-image", allowWrite: true},
		{name: "destructive action is refused", action: "TerminateCompShareCustomImage", allowWrite: true, wantErr: true},
		{name: "unclassified action is refused", action: "UnknownUpstreamAction", allowWrite: true, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := guardUpstreamContractWrite(tc.action, tc.flow, tc.allowWrite)
			if tc.wantErr && err == nil {
				t.Fatal("expected guard error")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected guard error: %v", err)
			}
		})
	}
}

func runUpstreamContractWorkflow(t *testing.T, ctx context.Context, executor tools.ToolExecutor, name string, params map[string]any, expectedStop string) {
	t.Helper()
	var def *workflow.Definition
	switch name {
	case "create-custom-image":
		def = workflow.CreateCustomImageDef()
	case "create-disk":
		def = workflow.CreateDiskDef()
	case "create-cfs":
		def = workflow.CreateCFSDef()
	default:
		t.Fatalf("unsupported workflow %q", name)
	}
	engine := workflow.NewEngine(executor, func(string, map[string]any) bool {
		return true
	}, func(event workflow.StepEvent) {
		t.Logf("step=%s status=%s", event.StepName, event.Status)
	})
	options := []workflow.RunOption{}
	if name == "create-cfs" {
		raw, err := executor.Execute(ctx, "DescribeCompShareSupportZone", map[string]any{})
		if err != nil {
			t.Fatalf("load live zone catalog: %v", err)
		}
		liveZones := zones.ParseSupportZones(raw)
		entries := make([]deployment.ZoneCatalogEntry, 0, len(liveZones))
		for _, zone := range liveZones {
			entries = append(entries, deployment.ZoneCatalogEntry{
				Placement: deployment.ZonePlacement{
					Zone: zone.Zone, Region: zone.Region, ZoneID: zone.ZoneID,
					AzGroup: zone.RegionID, IsPod: zone.IsPod,
				},
				DisplayName: zone.Describe,
			})
		}
		options = append(options, workflow.WithReferenceData(workflow.ReferenceData{
			ZoneCatalog: deployment.NewZoneCatalogSnapshot(true, entries),
		}))
	}
	result, err := engine.Run(ctx, def, params, options...)
	if err != nil {
		t.Fatalf("%s engine error: %v", name, err)
	}
	if expectedStop != "" {
		if result.Success || result.StoppedAt != expectedStop {
			t.Fatalf("%s result success=%v stopped_at=%q, want safe stop at %q: %s", name, result.Success, result.StoppedAt, expectedStop, result.Message)
		}
		t.Logf("%s safely stopped at %s: %s", name, result.StoppedAt, result.Message)
		return
	}
	if !result.Success {
		t.Fatalf("%s stopped at %s: %s", name, result.StoppedAt, result.Message)
	}
	safe := redactLiveContractResponse(result.Data)
	encoded, _ := json.MarshalIndent(safe, "", "  ")
	t.Logf("%s result:\n%s", name, encoded)
}

func redactLiveContractResponse(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, child := range typed {
			lowerKey := strings.ToLower(key)
			switch {
			case strings.Contains(lowerKey, "password"),
				strings.Contains(lowerKey, "token"),
				strings.Contains(lowerKey, "privatekey"),
				strings.Contains(lowerKey, "publickey"),
				strings.Contains(lowerKey, "email"),
				lowerKey == "url":
				out[key] = "[REDACTED]"
			default:
				out[key] = redactLiveContractResponse(child)
			}
		}
		return out
	case []any:
		out := make([]any, len(typed))
		for i := range typed {
			out[i] = redactLiveContractResponse(typed[i])
		}
		return out
	default:
		return value
	}
}
