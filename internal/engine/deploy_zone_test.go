package engine

import (
	"context"
	"errors"
	"testing"

	"github.com/compshare-agent/internal/llm"
	"github.com/compshare-agent/internal/tools"
	"github.com/compshare-agent/internal/zones"
)

// zoneCatalogExec returns the live 3-zone support-zone list for the catalog call
// and a benign result for anything else.
func zoneCatalogExec() *mockExecutorFn {
	return &mockExecutorFn{fn: func(action string, _ map[string]any) (map[string]any, error) {
		if action == "DescribeCompShareSupportZone" {
			return map[string]any{"ZoneInfo": []any{
				map[string]any{"Zone": "cn-wlcb-01", "Region": "cn-wlcb", "RegionId": float64(3001), "ZoneId": float64(10027), "Describe": "华北二A", "IsPod": false},
				map[string]any{"Zone": "cn-sh2-02", "Region": "cn-sh2", "RegionId": float64(3002), "ZoneId": float64(8200), "Describe": "上海二B"},
				map[string]any{"Zone": "cn-bj2-03", "Region": "cn-bj2", "RegionId": float64(3003), "ZoneId": float64(5001), "Describe": "华北一C", "IsPod": true},
			}}, nil
		}
		return map[string]any{"RetCode": float64(0)}, nil
	}}
}

func newZoneEngine(exec *mockExecutorFn, llmResp string) *Engine {
	eng := NewWithDeps(&mockLLM{responses: []llm.ChatResponse{{Content: llmResp}}}, exec,
		func(string, map[string]any) bool { return true })
	eng.zoneCatalog = zones.NewCatalog(0) // fresh, uncached per test
	return eng
}

func zoneUserCtx() context.Context {
	return tools.WithUser(context.Background(), tools.UserContext{TopOrganizationID: 1, OrganizationID: 2})
}

func TestResolveRequestedZone_ExactChineseName_NoLLM(t *testing.T) {
	eng := newZoneEngine(zoneCatalogExec(), "SHOULD-NOT-BE-USED")
	zone, clarify := eng.resolveRequestedZone(zoneUserCtx(), "用pytorch镜像部署一台华北一C的4090")
	if zone != "cn-bj2-03" || clarify != "" {
		t.Fatalf("got zone=%q clarify=%q, want cn-bj2-03 / no clarify", zone, clarify)
	}
	if eng.llmClient.(*mockLLM).callIdx != 0 {
		t.Errorf("an exact display-name match must not invoke the LLM (calls=%d)", eng.llmClient.(*mockLLM).callIdx)
	}
}

func TestResolveRequestedZone_ExactZoneId_NoLLM(t *testing.T) {
	eng := newZoneEngine(zoneCatalogExec(), "SHOULD-NOT-BE-USED")
	zone, clarify := eng.resolveRequestedZone(zoneUserCtx(), "在 cn-bj2-03 创建一台4090")
	if zone != "cn-bj2-03" || clarify != "" {
		t.Fatalf("got zone=%q clarify=%q, want cn-bj2-03", zone, clarify)
	}
}

func TestResolveRequestedZone_FuzzyMention_ClarifyViaLLM(t *testing.T) {
	// "华北一区" is partial (which zone in 华北一?) → the model asks to confirm 华北一C.
	eng := newZoneEngine(zoneCatalogExec(), `{"decision":"clarify","clarify":"您是指 华北一C 吗？"}`)
	zone, clarify := eng.resolveRequestedZone(zoneUserCtx(), "部署一台华北一区的4090")
	if zone != "" || clarify == "" {
		t.Fatalf("got zone=%q clarify=%q, want a clarify question", zone, clarify)
	}
}

func TestResolveRequestedZone_FuzzyMention_ExactViaLLM(t *testing.T) {
	// 华北 mention but no exact display-name substring → LLM resolves to cn-bj2-03.
	eng := newZoneEngine(zoneCatalogExec(), `{"decision":"exact","zone":"cn-bj2-03"}`)
	zone, clarify := eng.resolveRequestedZone(zoneUserCtx(), "帮我在华北新机房部署一个4090")
	if zone != "cn-bj2-03" || clarify != "" {
		t.Fatalf("got zone=%q clarify=%q, want cn-bj2-03", zone, clarify)
	}
}

func TestResolveRequestedZone_HallucinatedZoneFromLLM_DroppedToNone(t *testing.T) {
	// Model returns a zone not in the live list → never reaches the saga as a zone;
	// degrades to the alias floor (here empty — no alias in the message).
	eng := newZoneEngine(zoneCatalogExec(), `{"decision":"exact","zone":"cn-gd-99"}`)
	zone, clarify := eng.resolveRequestedZone(zoneUserCtx(), "帮我在华北新机房部署一个4090")
	if zone != "" || clarify != "" {
		t.Fatalf("hallucinated zone must be dropped, got zone=%q clarify=%q", zone, clarify)
	}
}

func TestResolveRequestedZone_NoZoneMention_NoLLM(t *testing.T) {
	eng := newZoneEngine(zoneCatalogExec(), "SHOULD-NOT-BE-USED")
	zone, clarify := eng.resolveRequestedZone(zoneUserCtx(), "部署一个qwen 32b")
	if zone != "" || clarify != "" {
		t.Fatalf("got zone=%q clarify=%q, want empty", zone, clarify)
	}
	if eng.llmClient.(*mockLLM).callIdx != 0 {
		t.Errorf("no zone mention must not invoke the LLM")
	}
}

func TestResolveRequestedZone_CatalogUnavailable_FallsBackToAliasFloor(t *testing.T) {
	exec := &mockExecutorFn{fn: func(string, map[string]any) (map[string]any, error) {
		return nil, errors.New("support-zone API down")
	}}
	eng := newZoneEngine(exec, "SHOULD-NOT-BE-USED")
	// "上海" is in the deterministic alias floor (extractDeployZone) → cn-sh2-02.
	zone, clarify := eng.resolveRequestedZone(zoneUserCtx(), "部署到上海的4090")
	if zone != "cn-sh2-02" || clarify != "" {
		t.Fatalf("got zone=%q clarify=%q, want cn-sh2-02 from the alias floor", zone, clarify)
	}
}

// applyCreateZoneResolution is the internal CreateInstanceWorkflow entry point: it
// mutates the LLM-built args (Zone override + ZoneDescribes injection) so a
// user-named zone resolves identically to the deploy saga. The LLM cannot know a
// new zone's id, so without this "华北一C" creates silently land in the default.

func TestResolveRequestedZone_NewlyAddedZone_NoCodeChange(t *testing.T) {
	// Future-proofing guarantee: a zone added upstream appears in
	// DescribeCompShareSupportZone and resolves with ZERO code change — exact
	// display-name / zone-id match reads the live catalog, no code references any
	// specific zone. Here a brand-new 华南一A (cn-gz-01) that this codebase has
	// never heard of (not in deployZoneAliases, not in any test fixture elsewhere).
	exec := &mockExecutorFn{fn: func(action string, _ map[string]any) (map[string]any, error) {
		if action == "DescribeCompShareSupportZone" {
			return map[string]any{"ZoneInfo": []any{
				map[string]any{"Zone": "cn-wlcb-01", "Region": "cn-wlcb", "ZoneId": float64(10027), "Describe": "华北二A"},
				map[string]any{"Zone": "cn-gz-01", "Region": "cn-gz", "ZoneId": float64(7001), "Describe": "华南一A"},
			}}, nil
		}
		return map[string]any{"RetCode": float64(0)}, nil
	}}
	eng := newZoneEngine(exec, "SHOULD-NOT-BE-USED")
	// By display name.
	zone, clarify := eng.resolveRequestedZone(zoneUserCtx(), "创建一台华南一A的4090")
	if zone != "cn-gz-01" || clarify != "" {
		t.Fatalf("new zone by name must resolve from the live catalog, no code change; got zone=%q clarify=%q", zone, clarify)
	}
	// By zone id.
	zone2, _ := eng.resolveRequestedZone(zoneUserCtx(), "部署到 cn-gz-01")
	if zone2 != "cn-gz-01" {
		t.Errorf("new zone by id must resolve; got %q", zone2)
	}
	// No LLM needed for an exact match (the Mentions fuzzy-gate is not even consulted).
	if eng.llmClient.(*mockLLM).callIdx != 0 {
		t.Errorf("exact new-zone resolution must not invoke the LLM (calls=%d)", eng.llmClient.(*mockLLM).callIdx)
	}
}

func TestResolveRequestedZone_NonexistentZone_ChallengesViaClarify(t *testing.T) {
	// A zone the user named that is NOT in the live catalog must challenge with a
	// clarify (the matcher states which zones ARE supported), not silently default.
	// "华北十区" doesn't exist but trips the zone-mention pre-filter (华北), so it
	// reaches the matcher — which returns the unsupported-zone clarify.
	eng := newZoneEngine(zoneCatalogExec(), `{"decision":"clarify","clarify":"当前仅支持 华北二A / 上海二B / 华北一C，请问您要用哪个可用区？"}`)
	zone, clarify := eng.resolveRequestedZone(zoneUserCtx(), "创建一台华北十区的4090")
	if zone != "" || clarify == "" {
		t.Fatalf("a non-existent zone must challenge with a clarify, got zone=%q clarify=%q", zone, clarify)
	}
}

func TestApplyCreateZoneResolution_ExactChineseName_OverridesZoneAndInjectsDescribes(t *testing.T) {
	eng := newZoneEngine(zoneCatalogExec(), "SHOULD-NOT-BE-USED")
	eng.lastUserMsg = "创建一台华北一C的4090实例"
	args := map[string]any{"Zone": "cn-wlcb-01", "GpuType": "4090"} // LLM echoed the documented default
	if clarify := eng.applyCreateZoneResolution(zoneUserCtx(), args); clarify != "" {
		t.Fatalf("an exact display name must not clarify, got %q", clarify)
	}
	if args["Zone"] != "cn-bj2-03" {
		t.Errorf("Zone not overridden to the resolved id: got %v, want cn-bj2-03", args["Zone"])
	}
	if args["ZoneIsPod"] != true {
		t.Errorf("ZoneIsPod not threaded from support-zone catalog: got %v, want true", args["ZoneIsPod"])
	}
	describes, _ := args["ZoneDescribes"].(map[string]string)
	if describes["cn-bj2-03"] != "华北一C" {
		t.Errorf("ZoneDescribes missing the console display name: got %v", describes)
	}
	if eng.llmClient.(*mockLLM).callIdx != 0 {
		t.Errorf("an exact display-name match must not invoke the LLM (calls=%d)", eng.llmClient.(*mockLLM).callIdx)
	}
}

func TestApplyCreateZoneResolution_FuzzyMention_ReturnsClarify_ZoneUntouched(t *testing.T) {
	eng := newZoneEngine(zoneCatalogExec(), `{"decision":"clarify","clarify":"您是指 华北一C 吗？"}`)
	eng.lastUserMsg = "创建一台华北一区的4090"
	args := map[string]any{"Zone": "cn-wlcb-01"}
	clarify := eng.applyCreateZoneResolution(zoneUserCtx(), args)
	if clarify == "" {
		t.Fatalf("a partial zone mention must return a clarify question instead of guessing")
	}
	if args["Zone"] != "cn-wlcb-01" {
		t.Errorf("Zone must stay untouched while clarifying, got %v", args["Zone"])
	}
	if _, ok := args["ZoneDescribes"]; ok {
		t.Errorf("ZoneDescribes must not be injected on the clarify short-circuit")
	}
}

func TestApplyCreateZoneResolution_NoZoneMention_LeavesZone_StillInjectsDescribes(t *testing.T) {
	eng := newZoneEngine(zoneCatalogExec(), "SHOULD-NOT-BE-USED")
	eng.lastUserMsg = "创建一台4090"
	args := map[string]any{"Zone": "cn-wlcb-01"}
	if clarify := eng.applyCreateZoneResolution(zoneUserCtx(), args); clarify != "" {
		t.Fatalf("no zone mention must not clarify, got %q", clarify)
	}
	if args["Zone"] != "cn-wlcb-01" {
		t.Errorf("Zone must stay as the LLM provided when no zone is named, got %v", args["Zone"])
	}
	if args["ZoneIsPod"] != false {
		t.Errorf("ZoneIsPod should be threaded for the retained zone: got %v, want false", args["ZoneIsPod"])
	}
	// Describes are still injected so the form labels whatever zone is shown.
	if describes, _ := args["ZoneDescribes"].(map[string]string); describes["cn-bj2-03"] != "华北一C" {
		t.Errorf("ZoneDescribes should be injected whenever the catalog is available, got %v", args["ZoneDescribes"])
	}
	if eng.llmClient.(*mockLLM).callIdx != 0 {
		t.Errorf("no zone mention must not invoke the LLM")
	}
}

func TestApplyCreateZoneResolution_CatalogUnavailable_NoOverrideNoDescribes(t *testing.T) {
	exec := &mockExecutorFn{fn: func(string, map[string]any) (map[string]any, error) {
		return nil, errors.New("support-zone API down")
	}}
	eng := newZoneEngine(exec, "SHOULD-NOT-BE-USED")
	eng.lastUserMsg = "创建一台华北一C的4090" // names a zone, but the catalog can't resolve it
	args := map[string]any{"Zone": "cn-wlcb-01"}
	if clarify := eng.applyCreateZoneResolution(zoneUserCtx(), args); clarify != "" {
		t.Fatalf("catalog-down must degrade silently, not clarify, got %q", clarify)
	}
	if args["Zone"] != "cn-wlcb-01" {
		t.Errorf("Zone must be untouched when the catalog is unavailable, got %v", args["Zone"])
	}
	if _, ok := args["ZoneDescribes"]; ok {
		t.Errorf("ZoneDescribes must not be injected without a live catalog")
	}
}
