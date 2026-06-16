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
				map[string]any{"Zone": "cn-wlcb-01", "Region": "cn-wlcb", "ZoneId": float64(10027), "Describe": "华北二A"},
				map[string]any{"Zone": "cn-sh2-02", "Region": "cn-sh2", "ZoneId": float64(8200), "Describe": "上海二B"},
				map[string]any{"Zone": "cn-bj2-03", "Region": "cn-bj2", "ZoneId": float64(5001), "Describe": "华北一C"},
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

func TestResolveDeployZone_ExactChineseName_NoLLM(t *testing.T) {
	eng := newZoneEngine(zoneCatalogExec(), "SHOULD-NOT-BE-USED")
	zone, clarify := eng.resolveDeployZone(zoneUserCtx(), "用pytorch镜像部署一台华北一C的4090")
	if zone != "cn-bj2-03" || clarify != "" {
		t.Fatalf("got zone=%q clarify=%q, want cn-bj2-03 / no clarify", zone, clarify)
	}
	if eng.llmClient.(*mockLLM).callIdx != 0 {
		t.Errorf("an exact display-name match must not invoke the LLM (calls=%d)", eng.llmClient.(*mockLLM).callIdx)
	}
}

func TestResolveDeployZone_ExactZoneId_NoLLM(t *testing.T) {
	eng := newZoneEngine(zoneCatalogExec(), "SHOULD-NOT-BE-USED")
	zone, clarify := eng.resolveDeployZone(zoneUserCtx(), "在 cn-bj2-03 创建一台4090")
	if zone != "cn-bj2-03" || clarify != "" {
		t.Fatalf("got zone=%q clarify=%q, want cn-bj2-03", zone, clarify)
	}
}

func TestResolveDeployZone_FuzzyMention_ClarifyViaLLM(t *testing.T) {
	// "华北一区" is partial (which zone in 华北一?) → the model asks to confirm 华北一C.
	eng := newZoneEngine(zoneCatalogExec(), `{"decision":"clarify","clarify":"您是指 华北一C 吗？"}`)
	zone, clarify := eng.resolveDeployZone(zoneUserCtx(), "部署一台华北一区的4090")
	if zone != "" || clarify == "" {
		t.Fatalf("got zone=%q clarify=%q, want a clarify question", zone, clarify)
	}
}

func TestResolveDeployZone_FuzzyMention_ExactViaLLM(t *testing.T) {
	// 华北 mention but no exact display-name substring → LLM resolves to cn-bj2-03.
	eng := newZoneEngine(zoneCatalogExec(), `{"decision":"exact","zone":"cn-bj2-03"}`)
	zone, clarify := eng.resolveDeployZone(zoneUserCtx(), "帮我在华北新机房部署一个4090")
	if zone != "cn-bj2-03" || clarify != "" {
		t.Fatalf("got zone=%q clarify=%q, want cn-bj2-03", zone, clarify)
	}
}

func TestResolveDeployZone_HallucinatedZoneFromLLM_DroppedToNone(t *testing.T) {
	// Model returns a zone not in the live list → never reaches the saga as a zone;
	// degrades to the alias floor (here empty — no alias in the message).
	eng := newZoneEngine(zoneCatalogExec(), `{"decision":"exact","zone":"cn-gd-99"}`)
	zone, clarify := eng.resolveDeployZone(zoneUserCtx(), "帮我在华北新机房部署一个4090")
	if zone != "" || clarify != "" {
		t.Fatalf("hallucinated zone must be dropped, got zone=%q clarify=%q", zone, clarify)
	}
}

func TestResolveDeployZone_NoZoneMention_NoLLM(t *testing.T) {
	eng := newZoneEngine(zoneCatalogExec(), "SHOULD-NOT-BE-USED")
	zone, clarify := eng.resolveDeployZone(zoneUserCtx(), "部署一个qwen 32b")
	if zone != "" || clarify != "" {
		t.Fatalf("got zone=%q clarify=%q, want empty", zone, clarify)
	}
	if eng.llmClient.(*mockLLM).callIdx != 0 {
		t.Errorf("no zone mention must not invoke the LLM")
	}
}

func TestResolveDeployZone_CatalogUnavailable_FallsBackToAliasFloor(t *testing.T) {
	exec := &mockExecutorFn{fn: func(string, map[string]any) (map[string]any, error) {
		return nil, errors.New("support-zone API down")
	}}
	eng := newZoneEngine(exec, "SHOULD-NOT-BE-USED")
	// "上海" is in the deterministic alias floor (extractDeployZone) → cn-sh2-02.
	zone, clarify := eng.resolveDeployZone(zoneUserCtx(), "部署到上海的4090")
	if zone != "cn-sh2-02" || clarify != "" {
		t.Fatalf("got zone=%q clarify=%q, want cn-sh2-02 from the alias floor", zone, clarify)
	}
}
