package engine

import (
	"strings"

	"github.com/compshare-agent/internal/knowledge"
)

// domainMatchGuardOn gates the #5 wrong-domain REFUSE arm. Default false =>
// byte-identical behavior: the domain verdict is still computed and recorded in
// the trace (AllCitedOffDomain / DomainInferenceEmpty), but the synthesis is
// never replaced with a refusal. Flipping it on is a separate, eval-gated PR —
// it must first prove 0 over-refusal, because an over-eager domain refusal
// would suppress legitimate answers whenever question-side product routing and
// chunk product_area tags disagree on a true match. Set once at boot from
// COMPSHARE_RAG_DOMAIN_MATCH_GUARD (cmd); the Go-package default stays false so
// engine/knowledge unit tests are unaffected.
var domainMatchGuardOn bool

// SetDomainMatchGuardEnabled toggles the #5 wrong-domain refuse arm. Boot-only
// (reversible by restart).
func SetDomainMatchGuardEnabled(v bool) { domainMatchGuardOn = v }

// DomainMatchGuardEnabled reports whether the refuse arm is on.
func DomainMatchGuardEnabled() bool { return domainMatchGuardOn }

// allCitedOffDomain reports whether EVERY judgeable evidence area is off the
// question's product area (the #5 wrong-domain signal), and whether the
// question area itself was inferable.
//
// Fail-safe by construction — it returns off=false (never flag/refuse) when:
//   - the question area could not be inferred (questionArea==""), or
//   - no evidence chunk declares a product_area (nothing judgeable).
//
// It returns off=true only when there is at least one judgeable area AND none of
// them matches the question area — i.e. the answer would ground entirely on
// off-domain evidence (a 库存 question grounded on billing chunks).
func allCitedOffDomain(questionArea string, areas []string) (off bool, inferredEmpty bool) {
	if strings.TrimSpace(questionArea) == "" {
		return false, true
	}
	judgeable := false
	onDomain := false
	for _, a := range areas {
		a = strings.TrimSpace(a)
		if a == "" {
			continue
		}
		judgeable = true
		if strings.EqualFold(a, questionArea) {
			onDomain = true
		}
	}
	if !judgeable {
		return false, false
	}
	return !onDomain, false
}

func hitProductAreas(hits []knowledge.RetrievalHit) []string {
	areas := make([]string, 0, len(hits))
	for _, h := range hits {
		areas = append(areas, h.Chunk.ProductArea)
	}
	return areas
}

func ledgerProductAreas(ledger knowledge.EvidenceLedger) []string {
	areas := make([]string, 0, len(ledger.Items))
	for _, item := range ledger.Items {
		areas = append(areas, item.ProductArea)
	}
	return areas
}
