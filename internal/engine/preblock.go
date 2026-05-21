package engine

import (
	"github.com/compshare-agent/internal/observability"
	"github.com/compshare-agent/internal/refusal"
	"github.com/compshare-agent/internal/router"

	openai "github.com/sashabaranov/go-openai"
)

// enginePreBlock is the package-level Chat()-head decision chain. Static
// because the rule list is stateless data; per-tenant rule overlays
// (when needed) should construct a fresh router.PreBlock at session
// scope rather than mutating this singleton.
//
// Order = evaluation order. Keyword sets are disjoint by construction
// (余额/账单/财务 vs 226604/资源不足 vs monitor-history regex), so the
// ordering does not affect correctness for any current real input —
// but it is preserved for trace stability and to keep ranking explicit
// if a future rule introduces overlap.
//
// When adding a new rule:
//
//  1. Add the Category + reply text in internal/refusal/templates.go.
//  2. Implement the predicate in this package (engine_*.go helpers).
//  3. Append a router.Rule literal here.
//  4. Update the priority-chain comment block above Engine.Chat().
//
// post-LLM hard-blocks (currently only cited_contract_violation in
// Chat()) are intentionally NOT routed through this chain — they are
// structurally different (run AFTER the LLM produces text, not BEFORE).
// When ≥2 post-LLM rules exist, factor them out to a sibling
// internal/policy/postblock.go using the same router.Rule pattern.
var enginePreBlock = router.New(
	router.Rule{
		Match:    isAccountBillingUnsupported,
		Category: refusal.CategoryAccountBilling,
		Reply:    refusal.AccountBillingUnsupported,
	},
	router.Rule{
		Match:    isResourceShortageQuestion,
		Category: refusal.CategoryResourceShortage,
		Reply:    refusal.ResourceShortage226604,
	},
	router.Rule{
		Match:    isUnsupportedHistoricalMonitorQuestion,
		Category: refusal.CategoryMonitorHistory,
		Reply:    refusal.MonitorHistoryUnsupported,
	},
)

// emitMonitorHistoryHardBlock centralizes the hard-block side-effects
// for the monitor_history category — emit the observer record and
// append the canned reply to history — so all pre-LLM routing branches
// produce identical trace output.
//
// Three call sites converge here:
//
//  1. Chat() head keyword match — goes through enginePreBlock.Decide(),
//     which emits its own observer fire with category from the rule
//     literal. That path does NOT call this helper directly; the
//     observer payload it emits is byte-equal to what this helper
//     emits (same category, same Hit=true).
//  2. tryPlannerDispatch → dispatch.Plan.Intent == IntentMonitorHistory
//     (engine.go). Without this helper, pre-PR #140-followup the
//     planner-classified path silently emitted the same reply but no
//     observer record — partial trace coverage. PR #140 review
//     finding fixed by routing through here.
//  3. tryPhase1Cutover → FallbackTimeWindow (engine.go). Same partial-
//     trace bug as path 2; same fix.
//
// Post-tool error paths (executeTool / friendlyToolErrorMessage with
// tools.ErrHistoricalMonitorUnsupported) are deliberately NOT routed
// through this helper — they have their own outcome-trace path and
// double-counting them as a pre-LLM hard-block would distort the
// downstream MySQL aggregation.
func (e *Engine) emitMonitorHistoryHardBlock() string {
	if e.hardBlockObserver != nil {
		e.hardBlockObserver(observability.EngineHardBlockTrace{
			Hit:      true,
			Category: refusal.CategoryMonitorHistory,
		})
	}
	e.messages = append(e.messages, openai.ChatCompletionMessage{
		Role:    openai.ChatMessageRoleAssistant,
		Content: refusal.MonitorHistoryUnsupported,
	})
	return refusal.MonitorHistoryUnsupported
}
