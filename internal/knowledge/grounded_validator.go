package knowledge

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/compshare-agent/internal/textutil"
)

// citeMarkerRE recognizes [[chunk_id]] markers, including spaced or unbalanced
// closing brackets produced by models. IDs cannot contain whitespace, brackets or
// newlines, so bare shell conditionals such as [[ -f file ]] remain ordinary prose.
// The entire marker is consumed to avoid leaking bracket residue.
var citeMarkerRE = regexp.MustCompile(`\[[ \t]*\[+\s*([^\[\]\s]+)\s*\]+(?:[ \t]*\])*`)

// positionalCiteRE parses an optional positional citation [n] (single bracket,
// 1-2 digits) that cites the n-th evidence item, 1-based in ledger order. It is
// scanned only AFTER the [[chunk_id]] markers are removed, so the inner digits of a
// [[id]] are never reread as a positional ref. An in-range [n] resolves to its ledger
// item; an out-of-range or prose [n] is ignored (not a fabricated citation) so a stray
// bracketed number cannot force a refusal.
var positionalCiteRE = regexp.MustCompile(`\[(\d{1,2})\]`)

// GroundedAnswerReport is the route-independent verdict for a free-text final
// answer validated against a per-turn ChunkID-keyed evidence ledger. It is
// produced by ValidateGroundedCitations and never mutates the answer.
type GroundedAnswerReport struct {
	// HasCitation is true when the answer carries >=1 [[chunk_id]] marker that
	// resolves to a ChunkID present in the ledger.
	HasCitation bool
	// CitedChunkIDs are the ledger ChunkIDs the answer cited, deduped, in
	// first-occurrence order.
	CitedChunkIDs []string
	// UnknownCitations are [[...]] markers that do NOT resolve to any ledger
	// ChunkID — a fabricated or garbled citation. Any non-empty value fails the
	// grounded contract, even on an otherwise-cited answer.
	UnknownCitations []string
}

// Grounded reports whether the answer is properly cited: it carries >=1 citation
// that resolves to a retrieved ChunkID AND no citation to an unknown ChunkID (a
// fabricated/garbled citation fails even an otherwise-cited answer). It does NOT
// decide the refusal exemption — whether an UNcited answer is an acceptable
// abstention is an engine-layer call, because a substantive answer that merely
// contains a hedge phrase must not be mistaken for a refusal. Scope note: this
// validates the CITATIONS that are present, not the grounding of uncited prose.
func (r GroundedAnswerReport) Grounded() bool {
	return r.HasCitation && len(r.UnknownCitations) == 0
}

// ValidateGroundedCitations extracts [[chunk_id]] markers from a final answer and
// classifies each against the per-turn evidence ledger. It is route-independent:
// any answer that consumed retrieval (terminal RAG, agentic SearchKnowledge,
// diagnosis) can be validated against its ledger with the same function. It never
// mutates the answer — strip the markers for display with StripCiteMarkers.
func ValidateGroundedCitations(answer string, ledger EvidenceLedger) GroundedAnswerReport {
	report := GroundedAnswerReport{}
	if strings.TrimSpace(answer) == "" {
		return report
	}
	known := make(map[string]struct{}, len(ledger.Items))
	for _, item := range ledger.Items {
		if id := strings.TrimSpace(item.ChunkID); id != "" {
			known[id] = struct{}{}
		}
	}
	seenKnown := map[string]struct{}{}
	seenUnknown := map[string]struct{}{}
	for _, m := range citeMarkerRE.FindAllStringSubmatch(answer, -1) {
		id := strings.TrimSpace(m[1])
		if id == "" {
			continue
		}
		if _, ok := known[id]; ok {
			report.HasCitation = true
			if _, dup := seenKnown[id]; !dup {
				seenKnown[id] = struct{}{}
				report.CitedChunkIDs = append(report.CitedChunkIDs, id)
			}
			continue
		}
		if _, dup := seenUnknown[id]; !dup {
			seenUnknown[id] = struct{}{}
			report.UnknownCitations = append(report.UnknownCitations, id)
		}
	}
	// Positional [n] citations (1-based into ledger order). Scanned on a copy with the
	// [[chunk_id]] markers removed so the inner digits of a [[id]] are not reread. An
	// in-range [n] cites ledger.Items[n-1]; out-of-range/prose [n] is ignored (NOT a
	// fabricated citation — avoids a stray bracketed number forcing a refusal).
	stripped := citeMarkerRE.ReplaceAllString(answer, "")
	for _, m := range positionalCiteRE.FindAllStringSubmatch(stripped, -1) {
		n, err := strconv.Atoi(m[1])
		if err != nil || n < 1 || n > len(ledger.Items) {
			continue
		}
		id := strings.TrimSpace(ledger.Items[n-1].ChunkID)
		if id == "" {
			continue
		}
		report.HasCitation = true
		if _, dup := seenKnown[id]; !dup {
			seenKnown[id] = struct{}{}
			report.CitedChunkIDs = append(report.CitedChunkIDs, id)
		}
	}
	return report
}

// citeStripPunct collapses the CJK/ASCII punctuation spacing left when a
// [[chunk_id]] marker that immediately preceded a sentence terminator is removed,
// mirroring the cosmetic tidy stripCitationMarkers applies for the [n] scheme.
var citeStripPunct = [][2]string{
	{" ，", "，"}, {" 。", "。"}, {" ；", "；"}, {" ：", "："},
	{" 、", "、"}, {" ！", "！"}, {" ？", "？"},
	{" ,", ","}, {" .", "."}, {" ;", ";"}, {" :", ":"},
	{" !", "!"}, {" ?", "?"},
}

// StripCiteMarkers removes [[chunk_id]] markers from the answer before it is shown
// to the user. The cited-chunk mapping survives in the GroundedAnswerReport (extracted
// before stripping). Cosmetic cleanup collapses the spaces/punctuation a removed
// marker leaves so the user reply is not ragged; newlines are preserved so
// markdown structure (lists, tables) survives.
// Cleanup runs only on prose so code remains byte-identical.
func StripCiteMarkers(answer string) string {
	if answer == "" {
		return answer
	}
	return textutil.MapOutsideCode(answer, stripCiteMarkersInProse)
}

func stripCiteMarkersInProse(prose string) string {
	out := citeMarkerRE.ReplaceAllString(prose, "")
	// Also strip positional [n] markers (1-2 digit single bracket) so a numbered
	// citation does not show in the user-facing reply, mirroring the terminal route's
	// stripCitationMarkers. Done after the [[chunk_id]] strip so [[id]] inner digits
	// were already consumed.
	out = positionalCiteRE.ReplaceAllString(out, "")
	for _, pair := range citeStripPunct {
		out = strings.ReplaceAll(out, pair[0], pair[1])
	}
	for strings.Contains(out, "  ") {
		out = strings.ReplaceAll(out, "  ", " ")
	}
	if strings.Contains(out, " \n") || strings.HasSuffix(out, " ") {
		lines := strings.Split(out, "\n")
		for i := range lines {
			lines[i] = strings.TrimRight(lines[i], " ")
		}
		out = strings.Join(lines, "\n")
	}
	return out
}
