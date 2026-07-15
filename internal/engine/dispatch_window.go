package engine

import (
	openai "github.com/sashabaranov/go-openai"

	"github.com/compshare-agent/internal/intent"
	"github.com/compshare-agent/internal/tools"
)

// visibleRegistryForIntentRoute is the single production seam that builds the
// ReAct dispatch tool window for a planner route. The window is derived SOLELY
// from the route's intent (intent.IntentToolSubset → tools.VisibleRegistryForScope).
//
// route.RequiredTools — the planner-emitted LLM field — is deliberately NOT read
// here: it is validation/trace-only and does not authorize dispatch (see the
// IntentRoute.RequiredTools doc comment). This is the seam that makes that
// contract test-enforceable: the real ReAct loop (engine.go) calls this for
// req.Tools, and TestPlannerRequiredToolsDoNotAuthorizeDispatch calls the SAME
// function, so wiring route.RequiredTools in here — e.g.
// tools.VisibleRegistryForSubset(route.RequiredTools, mutatingEnabled) — would
// fail that test instead of silently changing the dispatch surface.
//
// DENY BY DEFAULT. An intent with no explicit allowlist used to fall through
// tools.VisibleRegistryForSubset's `len(subset)==0` branch and receive the FULL
// registry — every mutating workflow tool included, because production runs with
// mutating enabled. That inverted the safety property the whole routing layer
// exists to provide: the router being LEAST certain about what the user wants
// produced the LARGEST tool window it could hand the model.
//
// It was not a corner case. Across 2167 de-duplicated production turns
// (2026-06-26..07-09) the intents that return no subset — knowledge_qa (55.7%),
// unknown (6.6%), the empty intent (2.5%), plus deploy_model / create_instance /
// billing_account_unsupported — are ~65% of all traffic. The single largest block
// is knowledge Q&A: a user asking what a GPU costs was being answered by a model
// holding StopInstanceWorkflow, RebootInstanceWorkflow, ResetPasswordWorkflow and
// CreateInstanceWorkflow. Only the confirmation gate stood between that and a
// write, and a confirmation gate is a prompt for the user, not an authorization
// boundary.
//
// So: an unrecognized intent is not authorized to WRITE. It still gets the full
// ordinary read-only registry, because unknown is also the catch-all lane that
// legitimately answers bare follow-ups ("嗯嗯", "那这个呢"). Knowledge retrieval is
// different: accepting it also requires the final answer to be checked against
// its evidence ledger. It is therefore named explicitly only for knowledge_qa.
// Diagnosis skills that need KB evidence use the orchestrator's separate virtual
// SearchKnowledge path with structured diagnosis-claim validation.
func visibleRegistryForIntentRoute(route intent.IntentRoute, mutatingEnabled bool) []openai.Tool {
	window := tools.VisibleRegistryForScope(toolScopeForIntent(route.Intent), mutatingEnabled)
	if route.Intent != intent.IntentKnowledgeQA {
		window = toolListWithoutFunction(window, "SearchKnowledge")
	}
	return window
}

// toolScopeForIntent resolves an intent to an EXPLICIT authorization, replacing the
// nil-means-everything sentinel. There is no "unset" here: every intent lands in
// exactly one named mode, so a new intent added without an allowlist fails closed
// (read-only) instead of silently inheriting write access.
func toolScopeForIntent(i intent.Intent) tools.ToolScope {
	scope := specForIntent(i).ToolScope
	scope.Names = append([]string(nil), scope.Names...)
	return scope
}
