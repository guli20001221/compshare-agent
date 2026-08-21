package engine

import (
	"time"

	"github.com/compshare-agent/internal/observability"
)

// TraceSnapshot is the final, content-free engine state needed to finish a
// trace after ChatWithOptions returns.
type TraceSnapshot struct {
	Registry                         observability.EntityRegistryTrace
	ReactRounds                      int
	RoundCeilingHit                  bool
	ActionProposalDisposition        string
	SessionState                     SessionState
	ContextVersion                   int
	SessionStateHydrated             bool
	ResolutionSource                 string
	SelectedInstanceIDAtStart        string
	SelectedInstanceSourceAtStart    string
	SelectedInstanceFreshnessAtStart string
	ContextSources                   []string
	ResponseContract                 string
	PromptSectionIDs                 []string
	EvidenceUpdateSource             string
	GroundingOutcome                 string
	PromptMessagesRawPeak            int
	PromptMessagesAssembledPeak      int
	PromptMessagesCapApplied         bool
}

func (e *Engine) TraceSnapshot(now time.Time) TraceSnapshot {
	if e == nil {
		return TraceSnapshot{}
	}
	state, version, hydrated := e.SessionStateSnapshot()
	return TraceSnapshot{
		Registry:                         e.RegistryTraceState(now),
		ReactRounds:                      e.ReactRoundsThisTurn(),
		RoundCeilingHit:                  e.ReactCeilingHitThisTurn(),
		ActionProposalDisposition:        e.ActionProposalDispositionThisTurn(),
		SessionState:                     state,
		ContextVersion:                   version,
		SessionStateHydrated:             hydrated,
		ResolutionSource:                 e.InstanceResolutionSource(),
		SelectedInstanceIDAtStart:        e.SelectedInstanceIDAtTurnStart(),
		SelectedInstanceSourceAtStart:    e.selectedInstanceSourceAtTurnStart,
		SelectedInstanceFreshnessAtStart: e.selectedInstanceFreshnessAtTurnStart,
		ContextSources:                   contextSourceIDs(e.turnContextViewThisTurn),
		ResponseContract:                 string(e.effectiveResponseContract()),
		PromptSectionIDs:                 append([]string(nil), e.promptSectionIDsThisTurn...),
		EvidenceUpdateSource:             normalizedEvidenceUpdateSource(e.verifiedEvidenceUpdateThisTurn),
		GroundingOutcome:                 normalizedGroundingOutcome(e.groundingOutcomeThisTurn),
		PromptMessagesRawPeak:            e.PromptMessagesRawPeak(),
		PromptMessagesAssembledPeak:      e.PromptMessagesAssembledPeak(),
		PromptMessagesCapApplied:         e.PromptMessagesCapApplied(),
	}
}
