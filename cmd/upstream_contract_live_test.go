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
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/compshare-agent/internal/config"
	"github.com/compshare-agent/internal/deployment"
	"github.com/compshare-agent/internal/tools"
	"github.com/compshare-agent/internal/workflow"
	"github.com/compshare-agent/internal/zones"
)

var (
	upstreamContractLive    = flag.Bool("upstream-contract-live", false, "run one real CompShare API action")
	upstreamContractConfig  = flag.String("upstream-contract-config", "", "config.yaml path; default deploy/conf/config.yaml")
	upstreamContractTopOrg  = flag.Uint("upstream-contract-top-org", 0, "top organization id")
	upstreamContractOrg     = flag.Uint("upstream-contract-org", 0, "organization id")
	upstreamContractProject = flag.String("upstream-contract-project", "", "project id")
	upstreamContractRegion  = flag.String("upstream-contract-region", "", "context region")
	upstreamContractEmail   = flag.String("upstream-contract-email", "", "gateway-authenticated user email")
	upstreamContractAccount = flag.Uint("upstream-contract-account", 0, "gateway-authenticated account/user id")
	upstreamContractAction  = flag.String("upstream-contract-action", "", "upstream action")
	upstreamContractArgs    = flag.String("upstream-contract-args", "{}", "JSON object with action arguments")
	upstreamContractFlow    = flag.String("upstream-contract-workflow", "", "run a workflow instead of a raw action (create-custom-image, create-disk, create-cfs)")
	upstreamContractStopAt  = flag.String("upstream-contract-expect-stop-at", "", "expect the workflow to stop safely at this step")
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

	root := behavioralRepoRoot(t)
	cfgPath := *upstreamContractConfig
	if cfgPath == "" {
		cfgPath = filepath.Join(root, "deploy", "conf", "config.yaml")
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
	roleURN, err := tools.RoleUrnFromTemplate(cfg.Agent.STS.RoleUrnTemplate, uint32(*upstreamContractTopOrg))
	if err != nil {
		t.Fatalf("role urn: %v", err)
	}
	ctx := tools.WithUser(context.Background(), tools.UserContext{
		TopOrganizationID: uint32(*upstreamContractTopOrg),
		OrganizationID:    uint32(*upstreamContractOrg),
		CompanyID:         uint32(*upstreamContractTopOrg),
		AccountID:         uint32(*upstreamContractAccount),
		RoleUrn:           roleURN,
		SessionName:       cfg.Agent.STS.DefaultSessionName,
		ProjectId:         cfg.Agent.ProjectId,
		Region:            cfg.Agent.Region,
		UserEmail:         strings.TrimSpace(*upstreamContractEmail),
	})
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
