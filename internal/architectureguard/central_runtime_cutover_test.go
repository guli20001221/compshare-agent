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
		// The central Agent owns semantic decisions. The retired create switch and
		// per-tier model router must not return as parallel routing centers.
		"COMPSHARE_UNIFIED_CREATE",
		"SetUnifiedCreateEnabled",
		"createFamilyIntent",
		"type ModelRouter struct",
		"NewModelRouter",
		"tier_routing",
		"AgentLLMClient",
		"agentLLMClient",
		// These zero-call compatibility helpers were removed rather than kept alive
		// by tests or comments.
		"func (e *Engine) semanticMemoryPrompt(",
		"func containsNormalizedKeyword(",
		"func (e *Engine) ClearContinuityAdvisories(",
		"func recentConversationExcerpts(",
		"func renderContinuityAdvisories(",
		"plannerTraceSupplier",
		"SetPlannerTrace",
		"plannerHandlerExecutor",
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
		"taskSlotSpecs",
		"TaskSlotUpdatesFromUserText",
		"TaskArgsFromSlots",
		"inferLifecycleAction",
		"createDiskSizeFromUserText",
		"PlannedExecutionPathForIntent",
		"USE_INTENT_SCOPED_REACT_PROMPT",
		"RenderIntentScopedReActCard",
		"ClassifyResetPasswordValue",
		"CreatePreferenceExtractor",
		"COMPSHARE_CREATE_PREF_EXTRACTOR",
		"SourceLegacyArguments",
		"observeLegacyWorkflowArguments",
		"startWithoutGPURequestedByText",
		"planWithUserTextMonitorMetrics",
		"augmentPlanTargetRefsFromUserText",
		// P6 orphan retirement (2026-07): the router-time boundary-pack
		// classification directives were deleted along with the intent router;
		// the package had no caller and only its own tests kept it "alive". Guard
		// against the symbol / package returning as live prompt input.
		"boundarypacks",
		"BoundaryPromptFragments",
		// P9 second-authorization deletion (2026-07): write-target authority is the
		// Resolver's dual proof (selection AND existence) alone. The workflow-layer
		// re-authorization center and its migration bridge were deleted; guard
		// against either returning to launder a target the Resolver never verified.
		"func (e *Engine) workflowTargetIsTrusted(",
		"func workflowTargetNameMentioned(",
		"executeWorkflowWithAuthority",
		"resolverAuthorized",
		"trustedWorkflowFrameActionThisTurn",
		"trustedWorkflowFrameTargetThisTurn",
		// FirstDecision retirement (2026-07): the forced-first-decision hop was
		// Codex's OFF-in-prod backfill that pre-called the model to seed a round-0
		// write proposal, narrowed the tool window after the first hop, and replayed
		// the seeded response. It never reliably carded a free-NL create and broke
		// multi-turn continuation, so it was deleted — the free ReAct loop is the sole
		// create path. Guard against any of its entry points returning to pre-empt the
		// Agent's own in-loop tool choice.
		"runForcedFirstDecision",
		"interpretFirstDecision",
		"SetForcedFirstDecisionEnabled",
		"COMPSHARE_FORCED_FIRST_DECISION",
		"seededFirstResponse",
		"writeWindowClosedThisTurn",
		"continueWithoutWriteTool",
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
