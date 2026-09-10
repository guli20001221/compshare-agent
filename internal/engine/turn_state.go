package engine

import (
	"github.com/compshare-agent/internal/agentruntime"
	"github.com/compshare-agent/internal/deployment"
	"github.com/compshare-agent/internal/knowledge"
	"github.com/compshare-agent/internal/observability"
)

// turnState is everything one user turn owns. ChatWithOptions replaces the
// whole value at entry, so a field added here starts every turn at its zero
// value by construction — there is no reset list that a later field can be
// left out of.
//
// It is embedded in Engine, so these stay ordinary e.fooThisTurn reads at their
// use sites. What the type adds is the lifetime: Engine now holds session state
// plus one turn, instead of holding both kinds of field side by side and
// telling them apart by a suffix.
//
// Two kinds of per-turn field deliberately stay on Engine, because their
// lifetime is "cleared when the call returns", not "replaced when the next call
// starts": currentCtx and currentTurnID. Their absence outside a turn is what
// stops an out-of-turn caller from inheriting a live context or an audit
// identity, and construction at the next entry would come too late for that.
type turnState struct {
	// zoneCatalogThisTurn is populated lazily and shared by the zone-catalog read
	// capability, CodecZone and workflows. Direct unit calls outside Chat remain
	// uncached so tests cannot accidentally share state.
	zoneCatalogThisTurn        *deployment.ZoneCatalogSnapshot
	agentRuntimeEventsThisTurn []agentruntime.Event
	readExpensiveCallsThisTurn int
	// searchKnowledgeHitsThisTurn holds the raw hits behind this turn's evidence
	// so the final-answer citation check runs against exactly what the agent was
	// shown. Whether retrieval ran at all is len(searchKnowledgeActivitiesThisTurn),
	// not a separate flag.
	searchKnowledgeHitsThisTurn []knowledge.RetrievalHit
	// answerEchoedChunkIDThisTurn names the chunk whose body the final answer
	// reproduced verbatim, or "" for none. TELEMETRY ONLY — it is carried into the
	// turn-aggregate retrieval trace and must never gate, rewrite or replace an
	// answer (see finalizeAgentLoopKnowledgeAnswer).
	answerEchoedChunkIDThisTurn string
	// readChunkIDsThisTurn records which ledger snippets hold a complete body
	// rather than a search excerpt, so a re-read re-projects that text instead of
	// fetching a second copy into context. How many reads the turn has spent is
	// counted from the transcript.
	readChunkIDsThisTurn map[string]struct{}
	// automaticKnowledgeBodyIDsThisTurn deduplicates automatic body-read attempts
	// across SearchKnowledge calls, including failed attempts. Each search has its
	// own bounded body batch, separate from the model-visible ReadChunk budget.
	automaticKnowledgeBodyIDsThisTurn map[string]struct{}
	// searchKnowledgeCapabilitiesThisTurn maps only model-visible chunk IDs to
	// the short-lived remote search_id that surfaced them. It is intentionally
	// engine-local: sharing it through the process-wide retriever would let one
	// user's capability authorize another user's evidence read.
	searchKnowledgeCapabilitiesThisTurn map[string]string
	// belowFloorKnowledgeIDsThisTurn marks only weak candidates SearchKnowledge
	// explicitly exposed for optional full-body review. They are not evidence
	// until ReadChunk succeeds, and then remain low-confidence.
	belowFloorKnowledgeIDsThisTurn map[string]struct{}
	// searchKnowledgeLedgerThisTurn is the per-turn ChunkID-keyed, deduped
	// evidence ledger: the union of every SearchKnowledge call's items this turn.
	// The grounded-answer validator accepts only ChunkIDs present here.
	searchKnowledgeLedgerThisTurn       knowledge.EvidenceLedger
	searchKnowledgeActivitiesThisTurn   []observability.RetrievalActivity
	searchKnowledgeActivityIDsByChunkID map[string][]string
	// directAnswerToolRetryPending is local to the current ReAct run. It keeps
	// the retry in the sole Agent loop and is never persisted as semantic state.
	directAnswerToolRetryPending bool
	// directAnswerToolRetryOutcomeThisTurn is content-free telemetry for the
	// bounded retry. Empty means the retry did not run.
	directAnswerToolRetryOutcomeThisTurn string
	// turnTokensConsumed accumulates tokenUsageTotal(usage) across every LLM call
	// within the current Chat() invocation. Read at ReAct loop iteration
	// boundaries to enforce maxTokensPerTurn — never mid tool_call / tool_result
	// pair.
	turnTokensConsumed int
	// reactRoundsThisTurn counts the ReAct loop rounds entered this turn (zero
	// for deterministic exits such as an explicit human handoff). reactCeilingHit
	// ThisTurn is set when the loop exhausted maxReActRounds without a final
	// answer (that path emits no hard-block, so the trace's budget terminus is
	// otherwise underivable). Both are read post-turn by the trace recorder via
	// ReactRoundsThisTurn / ReactCeilingHitThisTurn.
	reactRoundsThisTurn     int
	reactCeilingHitThisTurn bool
	// Context-assembler observability. Peak raw history size and peak assembled
	// request size across this turn's rounds, plus whether the conservative
	// message cap ever shed anything. Content-free; read post-turn via the
	// Prompt* accessors.
	promptMessagesRawPeakThisTurn       int
	promptMessagesAssembledPeakThisTurn int
	promptMessagesCapAppliedThisTurn    bool
	turnModelCallsThisTurn              int
	turnModelAttemptsThisTurn           []observability.ModelAttemptTrace
	turnCompletionClassHint             string
	turnCompletionReasonHint            string
	runtimeFinishReasonThisTurn         agentruntime.FinishReason
	turnCompletionEmittedThisTurn       bool
	// A post-LLM or token-budget block can be recovered later in the same turn.
	// Keep the standing bit so a successfully validated answer can overwrite the
	// earlier failure attribution instead of being stored as "blocked".
	hardBlockStandingThisTurn bool
	hardBlockTraceThisTurn    observability.EngineHardBlockTrace
	// verifiedInstanceEvidenceThisTurn is current-turn existence evidence for the
	// ActionProposal target verifier: exact instance IDs a resource read confirmed
	// THIS turn by the upstream response echoing the SAME id. Only a same-id-verified
	// resource_info response populates it — a Monitor/refund subject taken from the
	// pre-query registry snapshot does NOT, so an observed-but-unverified id can never
	// serve as a write ExistenceProof.
	verifiedInstanceEvidenceThisTurn map[string]struct{}
	// actionProposalDispositionThisTurn is a compact, value-free classification of
	// what the resolver did with this turn's write proposal — "confirmation" /
	// "intake_form" when it reached a card, else the reason it did not
	// ("rejected:<slot>=<kind>", "missing:<fields>", "dependency_failure",
	// "conflict:<slots>", "intake_form_unavailable", "resolve_error"). The
	// acceptance measurement reads it (via ActionProposalDispositionThisTurn) to
	// attribute why a create proposal did or did not card. "" when no proposal ran
	// this turn.
	actionProposalDispositionThisTurn string
	// platformReadEvidenceThisTurn is proof of facts returned by read tools. It
	// supports server-side grounding and authorization checks, but it never
	// renders a second user-facing answer: the Agent sees the same evidence and
	// writes the final Markdown itself.
	platformReadEvidenceThisTurn []platformReadEvidence
	// sensitiveRepliesThisTurn contains credentials intentionally withheld from
	// model context. The final delivery boundary emits each one once.
	sensitiveRepliesThisTurn []string
	// committedWriteRepliesThisTurn preserves truthful, model-free completion
	// text if narration fails after an upstream write has committed.
	committedWriteRepliesThisTurn []string
	// Tool progress is turn-local. Replaying an identical read cannot create new
	// evidence, so the runtime returns the prior observation and withdraws that
	// concrete capability on the next round instead of spending ten rounds on it.
	toolResultsByCallThisTurn map[string]string
	// lastUserMsg is the raw user message for the current turn. Read by
	// executeDiagnosis guards for signal matching. Never mutated mid-turn.
	lastUserMsg          string
	imageContextThisTurn string
	// knowledgeOnlyThisTurn is an execution-time authorization boundary for
	// public Q&A transports. The advertised tool window is not trusted as the
	// only guard because a model can emit an unadvertised tool name.
	knowledgeOnlyThisTurn bool
	// publicPlatformReadOnlyThisTurn is the slightly broader public-channel
	// authorization boundary. It remains narrower than the console's ordinary
	// read surface: only public catalog/inventory reads are allowed.
	publicPlatformReadOnlyThisTurn bool
	// feishuConsoleHandoffThisTurn changes only the model's completion contract
	// for a public Feishu Q&A turn.
	feishuConsoleHandoffThisTurn bool
	// feishuSupportRendererThisTurn is a delivery choice, independent of which
	// authorization scope wins when a client advertises multiple read modes.
	feishuSupportRendererThisTurn bool
	// turnContextViewThisTurn is the immutable execution-context projection shared
	// by target resolution and the Agent context card. It is rebuilt exactly once after
	// turn-entry expiry/refresh and before the current user message is appended.
	turnContextViewThisTurn AgentContext
	turnContextViewReady    bool
	// Bounded, content-free metadata for the turn trace.
	promptSectionIDsThisTurn       []string
	verifiedEvidenceUpdateThisTurn string
	groundingOutcomeThisTurn       string
	groundingCitationScopeThisTurn string
	// Per-turn instance-binding observability. Captured at turn
	// entry / refreshSystemPrompt, read post-turn by the trace recorder. Per-turn
	// by design — a shared value would attribute one tenant's binding to
	// another's turn.
	//   - selectedInstance*AtTurnStart: the carried identity, provenance and
	//     freshness at turn entry, before any mid-turn re-binding.
	//   - instanceResolutionSourceThisTurn: how the turn-start binding was
	//     determined (observability.ResolutionSource* — session_state /
	//     single_host / unresolved).
	selectedInstanceIDAtTurnStart        string
	selectedInstanceSourceAtTurnStart    string
	selectedInstanceFreshnessAtTurnStart string
	instanceResolutionSourceThisTurn     string
	// instanceOpsInterruptionIncludedInReplyThisTurn is set only when the
	// deterministic response composer has copied the canonical interrupted-run
	// report into this turn's final reply. The transport acknowledges delivery
	// after that reply is durably stored; generating a reply alone is not proof
	// that a disconnected client received it.
	instanceOpsInterruptionIncludedInReplyThisTurn bool
	// lastConfirmationTerminalReason is why the most recent authorization card in
	// this turn ended, in observability's closed-set spelling. It exists because
	// ConfirmFunc answers a bool, so every non-approval — the user declining, the
	// card timing out, the client going away — arrives at the call site as the
	// same false, and the reply then told a user who ran out of time that they had
	// cancelled. The reason is already computed for trace; this carries the same
	// value to the sentence the user reads. Written by the per-turn confirmation
	// wrapper immediately before it returns, read by the call site that is about
	// to phrase the refusal. Single-goroutine: the wrapper and the ReAct loop that
	// consumes it are the same goroutine.
	lastConfirmationTerminalReason string
	// verbatimBlocksThisTurn holds text that must reach the user byte-identical
	// (see deliverVerbatim) without ending the turn. Accumulated as tools
	// return it and composed in front of the Agent's reply at the turn exit.
	verbatimBlocksThisTurn []string
}

// newTurnState is the turn's only initializer. The non-zero values here are the
// ones a turn must not start blank with: the two grounding fields default to
// "nothing observed yet" rather than to a passing verdict, and the two caches
// are allocated so a caller cannot distinguish "no entry" from "not a turn".
func newTurnState(userMsg string, opts ChatOptions) turnState {
	return turnState{
		lastUserMsg:                      userMsg,
		imageContextThisTurn:             opts.ImageContext,
		verifiedEvidenceUpdateThisTurn:   evidenceUpdateNone,
		groundingOutcomeThisTurn:         "unavailable",
		verifiedInstanceEvidenceThisTurn: map[string]struct{}{},
		toolResultsByCallThisTurn:        map[string]string{},

		// Authorization reductions are derived once, here, rather than being
		// copied onto the engine and cleared again by a deferred assignment. A
		// turn that does not ask for a reduction gets the zero value, and no turn
		// can observe the previous turn's.
		knowledgeOnlyThisTurn:          opts.KnowledgeOnly,
		publicPlatformReadOnlyThisTurn: opts.PublicPlatformReadOnly && !opts.KnowledgeOnly,
		feishuConsoleHandoffThisTurn:   opts.FeishuConsoleHandoff,
		feishuSupportRendererThisTurn:  opts.PublicPlatformReadOnly || opts.FeishuConsoleHandoff,
	}
}

// promptScope is the request-scope reduction a system prompt is built under.
// Only a turn has one: InitWithContext and RehydrateHistory rebuild the prompt
// outside any turn — RehydrateHistory on the cold path of a pooled engine — and
// pass the zero value, so a public-channel completion contract cannot be
// inherited by whatever turn runs next on that engine.
type promptScope struct {
	feishuConsoleHandoff         bool
	feishuPublicPlatformReadOnly bool
}

func (e *Engine) turnPromptScope() promptScope {
	return promptScope{
		feishuConsoleHandoff:         e.feishuConsoleHandoffThisTurn,
		feishuPublicPlatformReadOnly: e.publicPlatformReadOnlyThisTurn,
	}
}
