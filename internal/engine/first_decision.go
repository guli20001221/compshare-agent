package engine

import (
	openai "github.com/sashabaranov/go-openai"

	"github.com/compshare-agent/internal/llm"
	"github.com/compshare-agent/internal/observability"
)

// Forced first-decision (create-first-hop fix).
//
// A P6 migration gap: when independent semantic decision models were removed
// (26fa5f15) the create "first hop" was left to free tool-calling, and the flash
// model reliably free-rolls read calls (image/gpu/stock/price/RAG) before ever
// proposing a write — so a create request never deterministically reaches the
// guided card. This restores a STRUCTURAL first hop without resurrecting a
// second model, an intent enum, a keyword table, or a route registry:
//
// On the first ReAct round, when mutating is enabled and the model supports
// tool_choice=required, the central Agent is forced to call exactly one of the
// catalog-generated Request* proposal tools OR the internal ContinueWithoutWrite
// tool — nothing else (no reads, no SearchKnowledge). The model's CHOICE stays
// its own semantic judgment; only the interleave is removed structurally:
//   - Request* → straight into the existing Resolver → GuidedIntake/Confirmation.
//   - ContinueWithoutWrite → every Request* tool is removed for the rest of the
//     turn, so a write proposal can only ever be the first decision.
//   - anything else (0, >1, or an unexpected tool) → fail closed.
//
// Degrades (marked, never counted as a structural guarantee) when required
// tool_choice is unsupported or mutating is off.

const (
	firstDecisionSkippedReadOnly     = "skipped_read_only"    // mutating off — no write path exists
	firstDecisionDegradedUnsupported = "degraded_unsupported" // required tool_choice unsupported by model/key
	firstDecisionContinue            = "continue"             // model chose continue-without-write
	firstDecisionFailMulti           = "fail_multi_tool"      // more than one tool call — fail closed
	firstDecisionFailUnknown         = "fail_unknown_tool"    // an unexpected tool — fail closed
	firstDecisionFailEmpty           = "fail_no_tool"         // required but zero tool calls — fail closed
	// a write proposal outcome is recorded as "request:<ToolName>".

	continueWithoutWriteAck = "已确认：本轮不提交写操作，继续用只读能力和知识检索作答。"
	firstDecisionFailReply  = "抱歉，我没能确定这次要做的操作，请再说一次您的需求（例如查询实例、创建实例、或开关机某台实例）。"
)

// forcedFirstDecisionOn gates the forced first-decision. Default OFF in Go code
// so the bare test harness (NewWithDeps defaults mutating + required-tool-choice
// on) keeps its free round-0 behavior; the deploy config ships it ON, and the
// real-model acceptance gates enable it explicitly. Boot-only, frozen via
// SetForcedFirstDecisionEnabled — never a per-session setter (process-global,
// set once at boot, like unifiedCreateOn), so it cannot leak across sessions.
var forcedFirstDecisionOn = false

// SetForcedFirstDecisionEnabled toggles the forced first-decision (boot-only).
func SetForcedFirstDecisionEnabled(v bool) { forcedFirstDecisionOn = v }

// ForcedFirstDecisionEnabled reports whether the forced first-decision is on.
func ForcedFirstDecisionEnabled() bool { return forcedFirstDecisionOn }

// applyFirstDecision interprets the forced first-decision response. It returns
// (cont, final, finalReply):
//   - cont=true  → continue-without-write chosen; run the rest of the turn with
//     Request* tools removed.
//   - final=true → fail closed; finalReply is the user-facing refusal.
//   - both false → a single Request* proposal; the caller falls through to the
//     normal tool-execution path (which runs executeActionProposal).
//
// It owns all message bookkeeping so the conversation history stays well-formed
// (every tool_call gets a tool response), and never streams the model's raw
// first-decision content to the user.
func (e *Engine) applyFirstDecision(resp *llm.ChatResponse, onStep func(StepEvent)) (cont bool, final bool, finalReply string) {
	if onStep == nil {
		onStep = func(StepEvent) {}
	}
	e.writeWindowClosedThisTurn = true

	if len(resp.ToolCalls) != 1 {
		outcome := firstDecisionFailMulti
		if len(resp.ToolCalls) == 0 {
			outcome = firstDecisionFailEmpty
		}
		e.firstDecisionOutcomeThisTurn = outcome
		e.appendFirstDecisionFailHistory(resp)
		onStep(StepEvent{Type: StepBlocked, Action: "FirstDecision", Source: observability.ToolSourceMainReAct, Message: outcome})
		return false, true, firstDecisionFailReply
	}

	tc := resp.ToolCalls[0]
	name := tc.Function.Name
	switch {
	case name == continueWithoutWriteName:
		e.firstDecisionOutcomeThisTurn = firstDecisionContinue
		e.messages = append(e.messages, openai.ChatCompletionMessage{
			Role: openai.ChatMessageRoleAssistant, Content: resp.Content, ToolCalls: resp.ToolCalls,
		})
		e.messages = append(e.messages, openai.ChatCompletionMessage{
			Role: openai.ChatMessageRoleTool, Content: continueWithoutWriteAck, ToolCallID: tc.ID,
		})
		onStep(StepEvent{Type: StepToolResult, Action: continueWithoutWriteName, Source: observability.ToolSourceMainReAct, Message: "首决策=不写，改用只读能力作答"})
		return true, false, ""
	case isProposalToolName(name):
		e.firstDecisionOutcomeThisTurn = "request:" + name
		// Fall through: the normal tool-execution path appends the assistant
		// message and runs executeActionProposal (Resolver → GuidedIntake /
		// Confirmation). writeWindowClosedThisTurn is already set, so no second
		// write proposal is possible later this turn.
		return false, false, ""
	default:
		e.firstDecisionOutcomeThisTurn = firstDecisionFailUnknown
		e.appendFirstDecisionFailHistory(resp)
		onStep(StepEvent{Type: StepBlocked, Action: name, Source: observability.ToolSourceMainReAct, Message: firstDecisionFailUnknown})
		return false, true, firstDecisionFailReply
	}
}

// appendFirstDecisionFailHistory keeps the conversation well-formed after a
// fail-closed first decision: the assistant tool_call message, a synthetic
// response for every tool_call, then the refusal as the final assistant turn.
func (e *Engine) appendFirstDecisionFailHistory(resp *llm.ChatResponse) {
	e.messages = append(e.messages, openai.ChatCompletionMessage{
		Role: openai.ChatMessageRoleAssistant, Content: resp.Content, ToolCalls: resp.ToolCalls,
	})
	for _, tc := range resp.ToolCalls {
		e.messages = append(e.messages, openai.ChatCompletionMessage{
			Role: openai.ChatMessageRoleTool, Content: "skipped", ToolCallID: tc.ID,
		})
	}
	e.messages = append(e.messages, openai.ChatCompletionMessage{
		Role: openai.ChatMessageRoleAssistant, Content: firstDecisionFailReply,
	})
}
