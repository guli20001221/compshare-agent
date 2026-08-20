package engine

import (
	"encoding/json"
	"strings"

	"github.com/compshare-agent/internal/actionresolver"
	"github.com/compshare-agent/internal/deployment"
	"github.com/compshare-agent/internal/envelope"
	"github.com/compshare-agent/internal/platform"
)

const imageSubjectIDPrefix = "image:"

// constrainCarriedImageCandidate keeps an Agent-proposed exact image id only when
// the id was actually visible in the bounded conversation or in a successful
// image-list observation from this turn. Live catalog existence is necessary but
// is not evidence that the Agent copied the id from an authorized source: without
// this gate, an invented-but-valid id could become the default merely because it
// happened to exist upstream.
//
// A current-user exact id is already grounded by deriveProposalProvenance and
// bypasses this candidate-only gate. Removing the id does not erase a separately
// proposed source: source remains an editable guided-flow input, while dropping it
// here would silently fall back to platform and could lose a valid community name.
func (e *Engine) constrainCarriedImageCandidate(
	proposal actionresolver.ActionProposal,
	view AgentContext,
) actionresolver.ActionProposal {
	candidate, ok := proposalSlotCandidate(proposal, "CompShareImageId")
	if !ok || candidate.Source == actionresolver.SourceUserExplicit {
		return proposal
	}
	id, ok := candidate.Value.(string)
	id = strings.TrimSpace(id)
	if !ok || id == "" {
		return proposal
	}
	if imageIDAppearsInAgentContext(view, id) || e.imageIDAppearsInCurrentReadEvidence(id) {
		return proposal
	}
	return discardCarriedImageCandidate(proposal)
}

// carriedImageMatchesExplicitName checks only the mixed-provenance case: the
// current user supplied a name, while the concrete id came from the Agent. The
// current name is the newer instruction. A verified historical id may remain as
// the suggested default only when the live catalog row is related to that name.
//
// The match delegates to the catalog's strict direct-name relation, not the
// picker's intentionally broad near-match ranking. It introduces no
// image-specific aliases or keyword table.
func carriedImageMatchesExplicitName(
	proposal actionresolver.ActionProposal,
	snapshot *deployment.ImageCatalogSnapshot,
) (hasExplicitName bool, matches bool) {
	idCandidate, hasID := proposalSlotCandidate(proposal, "CompShareImageId")
	if !hasID || idCandidate.Source == actionresolver.SourceUserExplicit {
		return false, false
	}
	id, ok := idCandidate.Value.(string)
	id = strings.TrimSpace(id)
	if !ok || id == "" {
		return false, false
	}

	nameCandidate, hasName := proposalSlotCandidate(proposal, "ImageName")
	if !hasName || nameCandidate.Source != actionresolver.SourceUserExplicit {
		return false, false
	}
	name, ok := nameCandidate.Value.(string)
	name = strings.TrimSpace(name)
	if !ok || name == "" {
		return false, false
	}
	if snapshot == nil || !snapshot.Available() {
		return true, false
	}
	entry, found := snapshot.ByID(id)
	if !found {
		return true, false
	}
	return true, deployment.DirectImageNameMatch(entry, name)
}

func discardCarriedImageCandidate(proposal actionresolver.ActionProposal) actionresolver.ActionProposal {
	out := proposal
	out.Slots = make([]actionresolver.SlotCandidate, 0, len(proposal.Slots))
	for _, candidate := range proposal.Slots {
		if candidate.Name == "CompShareImageId" {
			continue
		}
		out.Slots = append(out.Slots, candidate)
	}
	return out
}

func imageIDAppearsInAgentContext(view AgentContext, imageID string) bool {
	if containsStandaloneImageID(view.CurrentQuestion, imageID) {
		return true
	}
	for _, pair := range view.RecentConversation {
		if containsStandaloneImageID(pair.User, imageID) ||
			containsStandaloneImageID(pair.Assistant, imageID) {
			return true
		}
	}
	return false
}

func containsStandaloneValue(text, value string) bool {
	textRunes := []rune(text)
	valueRunes := []rune(strings.TrimSpace(value))
	if len(valueRunes) == 0 || len(valueRunes) > len(textRunes) {
		return false
	}
	for offset := 0; offset+len(valueRunes) <= len(textRunes); offset++ {
		end := offset + len(valueRunes)
		if string(textRunes[offset:end]) == string(valueRunes) &&
			standaloneSpan(textRunes, offset, end) {
			return true
		}
	}
	return false
}

// containsStandaloneImageID uses the image codec's opaque-identifier boundary:
// Chinese grammar may be adjacent to an ASCII id without becoming part of it.
// Keep the generic value helper above unchanged for ordinary words and names.
func containsStandaloneImageID(text, value string) bool {
	textRunes := []rune(text)
	valueRunes := []rune(strings.TrimSpace(value))
	if len(valueRunes) == 0 || len(valueRunes) > len(textRunes) {
		return false
	}
	for offset := 0; offset+len(valueRunes) <= len(textRunes); offset++ {
		end := offset + len(valueRunes)
		if string(textRunes[offset:end]) == string(valueRunes) &&
			opaqueIdentifierSpan(textRunes, offset, end) {
			return true
		}
	}
	return false
}

func (e *Engine) imageIDAppearsInCurrentReadEvidence(imageID string) bool {
	if e == nil {
		return false
	}
	for _, evidence := range e.platformReadEvidenceThisTurn {
		if imageEnvelopeContainsID(evidence.Envelope, imageID) {
			return true
		}
	}
	for _, raw := range e.toolResultsByCallThisTurn {
		var observation ReadCapabilityObservation
		if json.Unmarshal([]byte(raw), &observation) != nil ||
			observation.Status != platform.ReadStatusHandled ||
			observation.Envelope == nil {
			continue
		}
		if imageEnvelopeContainsID(*observation.Envelope, imageID) {
			return true
		}
	}
	return false
}

func imageEnvelopeContainsID(env envelope.Envelope, imageID string) bool {
	if env.Kind != envelope.KindImageList {
		return false
	}
	for _, subject := range env.Subjects {
		if subject.Type == envelope.SubjectImage &&
			subject.ID == imageSubjectIDPrefix+imageID {
			return true
		}
	}
	return false
}
