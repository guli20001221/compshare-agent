package architectureguard

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProductionCannotReEnableLegacySemanticRuntime(t *testing.T) {
	root, err := FindRepoRoot(".")
	if err != nil {
		t.Fatal(err)
	}
	forbidden := []string{
		"COMPSHARE_INTENT_ROUTER_MODE",
		"COMPSHARE_DIRECT_DISPATCH_INTENTS",
		"COMPSHARE_INTENT_ROUTER_STRUCTURED_OUTPUT",
		"SetCentralAgentRuntimeEnabled",
		"type IntentRouter struct",
		"IntentRouterLLM",
		"IntentRouterInput",
		"NewIntentRouter",
		"ContextDecisionLayer",
		"ResolveContextDecision",
		"SetContextDecisionLayer",
		"llmContextDecisionLayer",
		"IntentRouterResult",
		"func IntentToolSubset(",
		"func visibleRegistryForIntentRoute(",
		"func (e *Engine) tryRouteDispatch(",
		"func (e *Engine) tryResumeResourceSelection(",
		"func (e *Engine) tryDeployModel(",
		"func (e *Engine) tryOperationLifecycleDispatch(",
		"func (e *Engine) tryDirectLifecycleFromUserText(",
		"func (e *Engine) tryCFSWorkflowDispatch(",
	}
	for _, top := range []string{"cmd", "internal"} {
		err := filepath.WalkDir(filepath.Join(root, top), func(path string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if d.IsDir() || filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for _, token := range forbidden {
				if strings.Contains(string(data), token) {
					rel, _ := filepath.Rel(root, path)
					t.Errorf("production legacy semantic switch %q remains in %s", token, filepath.ToSlash(rel))
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}
