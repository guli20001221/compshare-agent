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
// Order = evaluation order. Current rules are conservative safety classifiers
// (jailbreak/off-topic); account-level billing is not keyword-blocked here and
// is handled from the planner intent instead. Ordering is preserved for trace
// stability and to keep ranking explicit if a future rule introduces overlap.
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
	// Human-agent transfer runs LAST in the preblock chain. A narrow
	// phrase whitelist (转人工 / 人工客服 / 联系人工 / 找人工 / 叫人工)
	// matches the explicit transfer-to-human intent so 人工智能 / 人工费 /
	// 人工成本 — which also contain "人工" — do NOT false-trigger the QR
	// reply. The QR image URL is byte-pinned in refusal.HumanAgentTransfer;
	// refresh the image by editing that constant only. Jailbreak/off-topic
	// run earlier so a message that is both an instruction-override and a
	// transfer request is classified as jailbreak first.
	inputguard.Rule{
		Match:    isHumanAgentTransferRequest,
		Category: refusal.CategoryHumanAgent,
		Reply:    refusal.HumanAgentTransfer,
	},
	// account_billing + existing_disk_attach keyword hard-blocks removed
	// (2026-06-10): the planner / agent loop handles these better. Billing
	// symptoms route to billing_instance; genuine account-data (余额/发票)
	// now short-circuits from the planner intent in emitAccountBillingHardBlock;
	// "挂载已有盘" reaches the create-disk workflow whose tool description
	// already states it only creates a new disk (不支持挂载已有盘).
	// Same pattern as the resource_shortage removal (#261).
)

// emitMonitorHistoryHardBlock centralizes legacy monitor-history refusal side
// effects. The main product path now supports single-instance history windows;
// this helper remains for older guard/error paths that still need a consistent
// trace and canned reply.
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

// emitAccountBillingHardBlock handles planner-classified account-level finance
// requests. It deliberately does not use keyword matching: the router owns the
// semantic split between unsupported account ledgers and supported instance
// pricing/billing/refund routes.
func (e *Engine) emitAccountBillingHardBlock() string {
	if e.hardBlockObserver != nil {
		e.hardBlockObserver(observability.EngineHardBlockTrace{
			Hit:         true,
			Category:    refusal.CategoryAccountBilling,
			TriggeredBy: observability.HardBlockTriggerPlannerIntent,
		})
	}
	e.messages = append(e.messages, openai.ChatCompletionMessage{
		Role:    openai.ChatMessageRoleAssistant,
		Content: refusal.AccountBillingUnsupported,
	})
	return refusal.AccountBillingUnsupported
}
