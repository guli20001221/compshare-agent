package engine

import (
	"context"

	"github.com/compshare-agent/internal/llm"
	"github.com/compshare-agent/internal/tools"
	"github.com/compshare-agent/internal/zones"
)

// zoneCatalogExec returns the live 3-zone support-zone list for the catalog call
// and a benign result for anything else. Shared by the zone-catalog engine tests.
func zoneCatalogExec() *mockExecutorFn {
	return &mockExecutorFn{fn: func(action string, _ map[string]any) (map[string]any, error) {
		if action == "DescribeCompShareSupportZone" {
			return map[string]any{"ZoneInfo": []any{
				map[string]any{"Zone": "cn-wlcb-01", "Region": "cn-wlcb", "RegionId": float64(3001), "ZoneId": float64(10027), "Describe": "华北二A", "IsPod": false},
				map[string]any{"Zone": "cn-sh2-02", "Region": "cn-sh2", "RegionId": float64(3002), "ZoneId": float64(8200), "Describe": "上海二B", "DisableImageSync": true},
				map[string]any{"Zone": "cn-bj2-03", "Region": "cn-bj2", "RegionId": float64(3003), "ZoneId": float64(5001), "Describe": "华北一C", "IsPod": true},
			}}, nil
		}
		return map[string]any{"RetCode": float64(0)}, nil
	}}
}

// newZoneEngine builds an engine whose zone catalog is served by exec, with a
// fresh (uncached) cache so each test observes its own fetches. llmResp seeds the
// mock model for the rare test that still drives one.
func newZoneEngine(exec *mockExecutorFn, llmResp string) *Engine {
	eng := NewWithDeps(&mockLLM{responses: []llm.ChatResponse{{Content: llmResp}}}, exec,
		func(string, map[string]any) bool { return true })
	eng.zoneCatalog = zones.NewCatalog(0) // fresh, uncached per test
	return eng
}

func zoneUserCtx() context.Context {
	return tools.WithUser(context.Background(), tools.UserContext{TopOrganizationID: 1, OrganizationID: 2})
}
