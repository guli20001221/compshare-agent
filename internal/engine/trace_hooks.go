package engine

import (
	"time"

	"github.com/compshare-agent/internal/governance"
	"github.com/compshare-agent/internal/llm"
	"github.com/compshare-agent/internal/observability"
)

// TraceHooks is the complete, transport-neutral observability surface for one
// Engine turn. Durable coordination uses this single seam rather than reaching
// into HTTP's recorder or depending on every individual Engine setter.
type TraceHooks struct {
	Retrieval     func(observability.RetrievalTrace)
	Freshness     func(observability.FreshnessTrace)
	Diagnosis     func(observability.DiagnosisTrace)
	Outcome       func(observability.OutcomeTrace)
	Renderer      func(observability.RendererTrace)
	HardBlock     func(observability.EngineHardBlockTrace)
	Completion    func(observability.TurnCompletionTrace)
	RateLimit     func(governance.Decision)
	TokenUsage    func(llm.TokenUsage)
	Authorization func(observability.AuthorizationTrace)
	Confirmation  func(observability.ConfirmationTrace)
}

// TraceSnapshot is the final, content-free engine state needed to finish a
// trace after ChatWithOptions returns.
type TraceSnapshot struct {
	Registry                    observability.EntityRegistryTrace
	ReactRounds                 int
	RoundCeilingHit             bool
	ActionProposalDisposition   string
	SessionState                SessionState
	ContextVersion              int
	SessionStateHydrated        bool
	ResolutionSource            string
	SelectedInstanceIDAtStart   string
	ContextSources              []string
	ResponseContract            string
	PromptSectionIDs            []string
	MemoryUpdateSource          string
	GroundingOutcome            string
	PromptMessagesRawPeak       int
	PromptMessagesAssembledPeak int
	PromptMessagesCapApplied    bool
}

// AttachTraceHooks replaces every per-turn observer as one atomic wiring step.
// Engines used by the durable coordinator are private to that turn, so there is
// no observer sharing between concurrent requests.
func (e *Engine) AttachTraceHooks(h TraceHooks) {
	if e == nil {
		return
	}
	e.SetRetrievalTraceObserver(h.Retrieval)
	e.SetFreshnessTraceObserver(h.Freshness)
	e.SetDiagnosisTraceObserver(h.Diagnosis)
	e.SetOutcomeTraceObserver(h.Outcome)
	e.SetRendererTraceObserver(h.Renderer)
	e.SetHardBlockObserver(h.HardBlock)
	e.SetTurnCompletionObserver(h.Completion)
	e.SetRateLimitObserver(h.RateLimit)
	e.SetTokenUsageObserver(h.TokenUsage)
	e.SetAuthorizationTraceObserver(h.Authorization)
	e.SetConfirmationTraceObserver(h.Confirmation)
}

func (e *Engine) TraceSnapshot(now time.Time) TraceSnapshot {
	if e == nil {
		return TraceSnapshot{}
	}
	state, version, hydrated := e.SessionStateSnapshot()
	return TraceSnapshot{
		Registry:                    e.RegistryTraceState(now),
		ReactRounds:                 e.ReactRoundsThisTurn(),
		RoundCeilingHit:             e.ReactCeilingHitThisTurn(),
		ActionProposalDisposition:   e.ActionProposalDispositionThisTurn(),
		SessionState:                state,
		ContextVersion:              version,
		SessionStateHydrated:        hydrated,
		ResolutionSource:            e.InstanceResolutionSource(),
		SelectedInstanceIDAtStart:   e.SelectedInstanceIDAtTurnStart(),
		ContextSources:              contextSourceIDs(e.turnContextViewThisTurn),
		ResponseContract:            string(e.effectiveResponseContract()),
		PromptSectionIDs:            append([]string(nil), e.promptSectionIDsThisTurn...),
		MemoryUpdateSource:          normalizedMemoryUpdateSource(e.memoryUpdateSourceThisTurn),
		GroundingOutcome:            normalizedGroundingOutcome(e.groundingOutcomeThisTurn),
		PromptMessagesRawPeak:       e.PromptMessagesRawPeak(),
		PromptMessagesAssembledPeak: e.PromptMessagesAssembledPeak(),
		PromptMessagesCapApplied:    e.PromptMessagesCapApplied(),
	}
}
