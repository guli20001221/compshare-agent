package engine

import (
	openai "github.com/sashabaranov/go-openai"

	"github.com/compshare-agent/internal/intent"
	"github.com/compshare-agent/internal/tools"
)

// visibleRegistryForIntentRoute is the single production seam that builds the
// ReAct dispatch tool window for a planner route. The window is derived SOLELY
// from the route's intent (intent.IntentToolSubset → tools.VisibleRegistryForSubset).
//
// route.RequiredTools — the planner-emitted LLM field — is deliberately NOT read
// here: it is validation/trace-only and does not authorize dispatch (see the
// IntentRoute.RequiredTools doc comment). This is the seam that makes that
// contract test-enforceable: the real ReAct loop (engine.go) calls this for
// req.Tools, and TestPlannerRequiredToolsDoNotAuthorizeDispatch calls the SAME
// function, so wiring route.RequiredTools in here — e.g.
// tools.VisibleRegistryForSubset(route.RequiredTools, mutatingEnabled) — would
// fail that test instead of silently changing the dispatch surface.
func visibleRegistryForIntentRoute(route intent.IntentRoute, mutatingEnabled bool) []openai.Tool {
	return tools.VisibleRegistryForSubset(intent.IntentToolSubset(route.Intent), mutatingEnabled)
}
