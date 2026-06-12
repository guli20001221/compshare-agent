package engine

import (
	"github.com/compshare-agent/internal/guardrails"
	"github.com/compshare-agent/internal/inputguard"
	"github.com/compshare-agent/internal/observability"
	"github.com/compshare-agent/internal/refusal"

	openai "github.com/sashabaranov/go-openai"
)

// enginePreBlock is the package-level Chat()-head decision chain. Static
// because the rule list is stateless data; per-tenant rule overlays
// (when needed) should construct a fresh inputguard.PreBlock at session
// scope rather than mutating this singleton.
//
// Order = evaluation order. Keyword sets are disjoint by construction
// (余额/账单/财务 vs monitor-history regex), so the
// ordering does not affect correctness for any current real input —
// but it is preserved for trace stability and to keep ranking explicit
// if a future rule introduces overlap.
//
// When adding a new rule:
//
//  1. Add the Category + reply text in internal/refusal/templates.go.
//  2. Implement the predicate in this package (engine_*.go helpers).
//  3. Append a inputguard.Rule literal here.
//  4. Update the priority-chain comment block above Engine.Chat().
//
// post-LLM hard-blocks (currently only cited_contract_violation in
// Chat()) are intentionally NOT routed through this chain — they are
// structurally different (run AFTER the LLM produces text, not BEFORE).
// When ≥2 post-LLM rules exist, factor them out to a sibling
// internal/policy/postblock.go using the same inputguard.Rule pattern.
var enginePreBlock = inputguard.New(
	// Jailbreak detection runs FIRST in the chain. If a user message
	// matches an instruction-override pattern we want to short-circuit
	// before any other rule (or the LLM) reads the payload; later rules
	// could otherwise eat the message ("ignore your billing rules" would
	// look billing-shaped to a keyword matcher) and emit the wrong
	// category. Detection itself is conservative-compound (verb + domain
	// noun BOTH required), so the false-positive cost of running first
	// is low.
	inputguard.Rule{
		Match:    guardrails.DetectJailbreakAttempt,
		Category: refusal.CategoryJailbreakAttempt,
		Reply:    refusal.JailbreakAttempt,
	},
	// Off-topic detection runs SECOND, after jailbreak. Off-topic asks
	// (political opinion, personal medical advice, investment
	// recommendations, severe-emotional-distress) get a redirect-style
	// refusal rather than going to the planner / LLM. Conservative-
	// compound predicates same shape as jailbreak — false-positive cost
	// kept low so benign platform questions never trip.
	inputguard.Rule{
		Match:    guardrails.DetectOffTopic,
		Category: refusal.CategoryOffTopic,
		Reply:    refusal.OffTopic,
	},
	// account_billing + existing_disk_attach keyword hard-blocks removed
	// (2026-06-10): the planner / agent loop handles these better. Billing
	// symptoms route to billing_instance; genuine account-data (余额/发票) to
	// console-guidance; "挂载已有盘" reaches the create-disk workflow whose tool
	// description already states it only creates a new disk (不支持挂载已有盘).
	// Same pattern as the resource_shortage removal (#261).
	inputguard.Rule{
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
//  3. tryRouteDispatch → FallbackTimeWindow (engine.go). Same partial-
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
			Hit:         true,
			Category:    refusal.CategoryMonitorHistory,
			TriggeredBy: observability.HardBlockTriggerPlannerIntent,
		})
	}
	e.messages = append(e.messages, openai.ChatCompletionMessage{
		Role:    openai.ChatMessageRoleAssistant,
		Content: refusal.MonitorHistoryUnsupported,
	})
	return refusal.MonitorHistoryUnsupported
}
