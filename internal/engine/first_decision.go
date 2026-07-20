package engine

import (
	"context"

	openai "github.com/sashabaranov/go-openai"

	"github.com/compshare-agent/internal/governance"
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
// second model, an intent enum, a keyword table, or a route registry.
//
// The forced decision runs as a small PRE-CALL, BEFORE the ReAct loop
// (agentruntime.Run), so it never consumes a normal ReAct round or the per-turn
// answer token budget (runForcedFirstDecision + engine.go). When mutating is
// enabled and the model supports tool_choice=required, the central Agent is
// forced (one bounded LLM call) to pick exactly one of the catalog-generated
// Request* proposal tools OR the internal ContinueWithoutWrite tool — nothing
// else (no reads, no SearchKnowledge). The model's CHOICE stays its own semantic
// judgment; only the interleave is removed structurally:
//   - Request* → seeded into ReAct round 0, which runs it through the existing
//     Resolver → GuidedIntake/Confirmation path verbatim (including its
//     non-terminal error/prose continuations).
//   - ContinueWithoutWrite → writeWindowClosedThisTurn is set; the loop drops
//     every Request* tool for the rest of the turn. No history is polluted and no
//     user-visible step is emitted, so the answer round rebuilds the identical
//     pre-decision snapshot.
//   - anything else (0, >1, or an unexpected tool) → fail closed (terminal).
//
// Degrades (marked, never counted as a structural guarantee) when required
// tool_choice is unsupported, when the provider silently drops the forcing and
// retries auto (llm.ChatResponse.ForcedToolChoiceDegraded), when the probe
// errors, or when mutating is off — in every degrade case the normal full-window
// loop runs exactly as if the flag were off.

const (
	firstDecisionSkippedReadOnly          = "skipped_read_only"          // mutating off — no write path exists
	firstDecisionDegradedUnsupported      = "degraded_unsupported"       // required tool_choice unsupported by model capability
	firstDecisionDegradedProviderFallback = "degraded_provider_fallback" // provider silently dropped forcing, retried auto
	firstDecisionDegradedError            = "degraded_error"             // the forced probe call itself errored
	firstDecisionContinue                 = "continue"                   // model chose continue-without-write
	firstDecisionFailMulti                = "fail_multi_tool"            // more than one tool call — fail closed
	firstDecisionFailUnknown              = "fail_unknown_tool"          // an unexpected tool — fail closed
	firstDecisionFailEmpty                = "fail_no_tool"               // required but zero tool calls — fail closed
	// a write proposal outcome is recorded as "request:<ToolName>".

	firstDecisionFailReply = "抱歉，我没能确定这次要做的操作，请再说一次您的需求（例如查询实例、创建实例、或开关机某台实例）。"
)

// forcedFirstDecisionOn gates the forced first-decision. Default OFF in Go code
// so the bare test harness (NewWithDeps defaults mutating + required-tool-choice
// on) keeps its free round-0 behavior. It is NOT yet enabled in the deploy config
// (deploy/conf/config.yaml leaves agent.features.forced_first_decision unset) —
// production enablement is deferred until the create->card acceptance passes; the
// real-model acceptance gates enable it explicitly for a run. Boot-only, frozen
// via SetForcedFirstDecisionEnabled — never a per-session setter (process-global,
// set once at boot, like unifiedCreateOn), so it cannot leak across sessions.
var forcedFirstDecisionOn = false

// SetForcedFirstDecisionEnabled toggles the forced first-decision (boot-only).
func SetForcedFirstDecisionEnabled(v bool) { forcedFirstDecisionOn = v }

// ForcedFirstDecisionEnabled reports whether the forced first-decision is on.
func ForcedFirstDecisionEnabled() bool { return forcedFirstDecisionOn }

// firstDecisionKind is the classified shape of a forced first-decision response.
type firstDecisionKind int

const (
	firstDecisionKindContinue firstDecisionKind = iota
	firstDecisionKindProposal
	firstDecisionKindFailClosed
)

// firstDecisionResult is the pure classification of the forced response. seed is
// set only for a proposal (the response to replay into round 0); reply only for
// fail-closed (the user-facing refusal).
type firstDecisionResult struct {
	outcome string
	kind    firstDecisionKind
	seed    *llm.ChatResponse
	reply   string
}

// interpretFirstDecision classifies the forced response with NO side effects
// (no message mutation, no observer, no engine-state write) — the caller owns
// those. A response with exactly one Request* tool is a write proposal; exactly
// one ContinueWithoutWrite is continue; anything else fails closed.
func interpretFirstDecision(resp *llm.ChatResponse) firstDecisionResult {
	if resp == nil || len(resp.ToolCalls) != 1 {
		outcome := firstDecisionFailMulti
		if resp == nil || len(resp.ToolCalls) == 0 {
			outcome = firstDecisionFailEmpty
		}
		return firstDecisionResult{outcome: outcome, kind: firstDecisionKindFailClosed, reply: firstDecisionFailReply}
	}
	name := resp.ToolCalls[0].Function.Name
	switch {
	case name == continueWithoutWriteName:
		return firstDecisionResult{outcome: firstDecisionContinue, kind: firstDecisionKindContinue}
	case isProposalToolName(name):
		return firstDecisionResult{outcome: "request:" + name, kind: firstDecisionKindProposal, seed: resp}
	default:
		return firstDecisionResult{outcome: firstDecisionFailUnknown, kind: firstDecisionKindFailClosed, reply: firstDecisionFailReply}
	}
}

// runForcedFirstDecision performs the structural first hop BEFORE the ReAct loop.
// It returns (handled, reply): handled=true ONLY for the terminal fail-closed
// refusal (reply is what the turn returns). Every other path returns
// handled=false and lets the loop run:
//   - continue-without-write sets writeWindowClosedThisTurn (loop drops Request*);
//   - a write proposal is stashed in seededFirstResponse for round 0;
//   - degraded / not-applicable leave both unset so the normal full-window loop
//     runs exactly as if the flag were off.
//
// It never charges the normal ReAct round count or the per-turn answer token
// budget, and never pollutes e.messages on the continue path (so the answer round
// rebuilds the byte-identical pre-decision snapshot). The probe IS admitted
// through the LLM rate-limit class: a write turn's round 0 is seeded and makes no
// further model call, so an unlimited probe would otherwise bypass the limiter.
func (e *Engine) runForcedFirstDecision(ctx context.Context, opts ChatOptions, onStep func(StepEvent)) (handled bool, reply string) {
	if onStep == nil {
		onStep = func(StepEvent) {}
	}
	if !forcedFirstDecisionOn {
		return false, ""
	}
	if !e.mutatingToolsEnabled {
		// No write path exists — nothing to force. Recorded for observability.
		e.firstDecisionOutcomeThisTurn = firstDecisionSkippedReadOnly
		return false, ""
	}
	if !e.supportsRequiredToolChoice {
		// The model capability flag says required tool_choice is unsupported — do
		// not force; the normal loop runs. Degrade, not a structural guarantee.
		e.firstDecisionOutcomeThisTurn = firstDecisionDegradedUnsupported
		return false, ""
	}
	if decision, ok := e.allowRateLimited(governance.ClassLLM, "forced_first_decision"); !ok {
		e.markTurnCompletion(observability.CompletionClassSafetyBlock, observability.CompletionReasonRateLimit)
		content := rateLimitMessage(decision.Reason)
		e.messages = append(e.messages, openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, Content: content})
		return true, content
	}
	req := llm.ChatRequest{
		Messages:   e.buildMessagesForLLM(),
		Tools:      firstDecisionToolWindow(),
		ToolChoice: "required",
	}
	resp, err := e.llmClient.Chat(ctx, req)
	if err != nil || resp == nil {
		// The probe failed outright (or returned no response). Degrade to the normal
		// loop (full window); the answer path surfaces any genuine LLM outage through
		// its own recovery. The resp==nil guard keeps observeFirstDecisionTokens /
		// interpretFirstDecision below from dereferencing a nil response.
		e.firstDecisionOutcomeThisTurn = firstDecisionDegradedError
		return false, ""
	}
	// Record the probe's cost for trace honesty WITHOUT charging the per-turn
	// answer token budget (turnTokensConsumed).
	e.observeFirstDecisionTokens(resp.Usage)
	if resp.ForcedToolChoiceDegraded {
		// The provider silently dropped the forcing and retried auto: this response
		// is not an authoritative first decision. Discard it and run the normal
		// full-window loop (as if the flag were off). Marked, never structural.
		e.firstDecisionOutcomeThisTurn = firstDecisionDegradedProviderFallback
		return false, ""
	}
	res := interpretFirstDecision(resp)
	e.firstDecisionOutcomeThisTurn = res.outcome
	switch res.kind {
	case firstDecisionKindContinue:
		// Internal control only: no history pollution, no user-visible step. The
		// loop's round 0 rebuilds the byte-identical snapshot with Request* removed.
		e.writeWindowClosedThisTurn = true
		return false, ""
	case firstDecisionKindProposal:
		// Seed round 0 with the pre-decided proposal so it runs through the exact
		// existing tool-execution path. writeWindowClosedThisTurn blocks any second
		// proposal later this turn.
		e.writeWindowClosedThisTurn = true
		e.seededFirstResponse = resp
		return false, ""
	default: // firstDecisionKindFailClosed
		// Terminal. Only the refusal enters history (hot == cold: the DB persists
		// user + final refusal, so the in-memory turn must match — no tool_call
		// scaffolding that a cold rehydrate would never see).
		onStep(StepEvent{Type: StepBlocked, Action: "FirstDecision", Source: observability.ToolSourceMainReAct, Message: res.outcome})
		e.messages = append(e.messages, openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, Content: res.reply})
		if opts.OnTextDelta != nil {
			opts.OnTextDelta(res.reply)
		}
		return true, res.reply
	}
}

// observeFirstDecisionTokens records the forced probe's token usage on the trace
// observer WITHOUT adding it to turnTokensConsumed — the probe is system overhead
// that must not reduce the answer's per-turn token budget. Overall cost stays
// honest because the observer feeds the trace's total_tokens.
func (e *Engine) observeFirstDecisionTokens(usage llm.TokenUsage) {
	if tokenUsageTotal(usage) == 0 {
		return
	}
	if e.tokenUsageObserver != nil {
		e.tokenUsageObserver(usage)
	}
}
