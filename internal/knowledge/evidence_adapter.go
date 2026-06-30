package knowledge

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

const DefaultEvidenceLedgerMaxItems = 3

// DefaultEvidenceSnippetMaxRunes bounds the per-item content excerpt carried by
// the SUBSTANTIVE ledger (BuildSubstantiveEvidenceLedger). It is large enough to
// capture the actionable head of an external runbook chunk (the flags/commands
// live in the first lines) yet bounded so a 3-item ledger fed back through the
// multi-round ReAct loop does not bloat the input (memory:
// priortext-avalanche-invalidates-planner).
const DefaultEvidenceSnippetMaxRunes = 400

// EvidenceLedger is the safe view of retrieval results for body-read agent
// skills. The diagnosis lane (BuildEvidenceLedger) intentionally does not expose
// KBChunk.Content because there the instance data is the primary evidence and the
// ledger is supplementary. The agentic-RAG registry tool (P3) instead uses
// BuildSubstantiveEvidenceLedger, which fills Snippet with a bounded content
// excerpt — on a symptom tool-ops turn the retrieved evidence IS the primary
// base, so a content-free ledger could not ground a real fix.
type EvidenceLedger struct {
	Query string         `json:"query,omitempty"`
	Items []EvidenceItem `json:"items"`
}

type EvidenceItem struct {
	// RefID is a turn-scoped citation handle ("1", "2", ...). It is distinct
	// from ChunkID, which is the durable corpus identifier persisted for audit.
	RefID   string `json:"ref_id,omitempty"`
	ChunkID string `json:"chunk_id"`
	Title   string `json:"title,omitempty"`
	// ProductArea is the chunk's declared product_area (KBChunk.ProductArea),
	// carried so the #5 wrong-domain guard can compare the cited evidence's
	// domain against the question's inferred area. Empty when undeclared.
	// json:"-" deliberately: this is internal plumbing for the guard, NOT part
	// of the agent-visible SearchKnowledge tool result — keeping it out of the
	// JSON leaves what the agent reads byte-identical (the agent loop is
	// default-on in production).
	ProductArea string `json:"-"`
	SourceType  string `json:"source_type,omitempty"`
	ScoreBucket string `json:"score_bucket,omitempty"`
	Summary     string `json:"summary"`
	// Snippet is a bounded excerpt of the chunk body. Empty on the content-free
	// diagnosis-lane ledger; populated by BuildSubstantiveEvidenceLedger so the
	// agent can ground an actionable answer. The no-raw-leak guard
	// (ValidateNoRawEvidenceLeak) still runs on the FINAL answer, so the agent
	// must paraphrase/cite rather than echo a >=32-rune verbatim passage.
	Snippet string `json:"snippet,omitempty"`
}

const (
	DiagnosisClaimSupported   = "supported"
	DiagnosisClaimInferred    = "inferred"
	DiagnosisClaimUnconfirmed = "unconfirmed"
)

type DiagnosisClaim struct {
	Claim    string   `json:"claim"`
	Status   string   `json:"status"`
	ChunkIDs []string `json:"chunk_ids,omitempty"`
	Reason   string   `json:"reason,omitempty"`
}

func (l EvidenceLedger) Empty() bool {
	return len(l.Items) == 0
}

func BuildEvidenceLedger(query string, hits []RetrievalHit, maxItems int) EvidenceLedger {
	if maxItems <= 0 {
		maxItems = DefaultEvidenceLedgerMaxItems
	}
	ledger := EvidenceLedger{
		Query: strings.TrimSpace(query),
		Items: []EvidenceItem{},
	}
	for _, hit := range hits {
		if !hit.Kept {
			continue
		}
		chunkID := strings.TrimSpace(hit.Chunk.ChunkID)
		if chunkID == "" {
			continue
		}
		title := clipRunes(compactWhitespace(hit.Chunk.Title), 80)
		summary := "Matched platform knowledge entry."
		if title != "" {
			summary = "Matched platform knowledge entry: " + title
		}
		ledger.Items = append(ledger.Items, EvidenceItem{
			ChunkID:     chunkID,
			Title:       title,
			ProductArea: strings.TrimSpace(hit.Chunk.ProductArea),
			SourceType:  clipRunes(compactWhitespace(hit.Chunk.SourceType), 40),
			ScoreBucket: evidenceScoreBucket(hit.Score),
			Summary:     clipRunes(summary, 160),
		})
		if len(ledger.Items) >= maxItems {
			break
		}
	}
	return ledger
}

// BuildSubstantiveEvidenceLedger is the agentic-RAG (P3) projection: identical to
// BuildEvidenceLedger but each item carries a bounded Snippet excerpt of the
// chunk body so the agent can synthesize an ACTIONABLE answer on a symptom
// tool-ops turn (where the retrieved evidence is the primary, not supplementary,
// base). snippetMaxRunes<=0 uses DefaultEvidenceSnippetMaxRunes. The Snippet is
// content the agent reads; the no-raw-leak guard still runs on the final answer.
func BuildSubstantiveEvidenceLedger(query string, hits []RetrievalHit, maxItems, snippetMaxRunes int) EvidenceLedger {
	if snippetMaxRunes <= 0 {
		snippetMaxRunes = DefaultEvidenceSnippetMaxRunes
	}
	ledger := BuildEvidenceLedger(query, hits, maxItems)
	byID := map[string]string{}
	for _, hit := range hits {
		id := strings.TrimSpace(hit.Chunk.ChunkID)
		if id == "" {
			continue
		}
		if _, ok := byID[id]; ok {
			continue
		}
		byID[id] = clipRunes(compactWhitespace(hit.Chunk.Content), snippetMaxRunes)
	}
	for i := range ledger.Items {
		ledger.Items[i].Snippet = byID[ledger.Items[i].ChunkID]
	}
	return ledger
}

func MergeEvidenceLedgers(first, second EvidenceLedger, maxItems int) EvidenceLedger {
	if maxItems <= 0 {
		maxItems = DefaultEvidenceLedgerMaxItems
	}
	out := EvidenceLedger{
		Query: strings.TrimSpace(first.Query),
		Items: []EvidenceItem{},
	}
	if q := strings.TrimSpace(second.Query); q != "" {
		if out.Query == "" {
			out.Query = q
		} else if out.Query != q {
			out.Query += " | " + q
		}
	}
	seen := map[string]struct{}{}
	for _, ledger := range []EvidenceLedger{first, second} {
		for _, item := range ledger.Items {
			id := strings.TrimSpace(item.ChunkID)
			if id == "" {
				continue
			}
			if _, ok := seen[id]; ok {
				continue
			}
			seen[id] = struct{}{}
			item.ChunkID = id
			out.Items = append(out.Items, item)
			if len(out.Items) >= maxItems {
				return out
			}
		}
	}
	return out
}

func ValidateDiagnosisClaims(claims []DiagnosisClaim, ledger EvidenceLedger) ([]DiagnosisClaim, error) {
	known := map[string]struct{}{}
	for _, item := range ledger.Items {
		id := strings.TrimSpace(item.ChunkID)
		if id != "" {
			known[id] = struct{}{}
		}
	}
	validated := make([]DiagnosisClaim, 0, len(claims))
	for i, claim := range claims {
		text := compactWhitespace(claim.Claim)
		if text == "" {
			continue
		}
		status := strings.ToLower(strings.TrimSpace(claim.Status))
		if status == "" {
			status = DiagnosisClaimUnconfirmed
		}
		switch status {
		case DiagnosisClaimSupported, DiagnosisClaimInferred, DiagnosisClaimUnconfirmed:
		default:
			return nil, fmt.Errorf("diagnosis claim %d has invalid status %q", i, claim.Status)
		}
		ids := dedupeStrings(trimStrings(claim.ChunkIDs))
		for _, id := range ids {
			if _, ok := known[id]; !ok {
				return nil, fmt.Errorf("diagnosis claim %d references unknown chunk_id %q", i, id)
			}
		}
		reason := compactWhitespace(claim.Reason)
		if status == DiagnosisClaimSupported && len(ids) == 0 {
			status = DiagnosisClaimUnconfirmed
			reason = appendDiagnosisClaimReason(reason, "supported status had no chunk_ids; downgraded to unconfirmed")
		}
		validated = append(validated, DiagnosisClaim{
			Claim:    clipRunes(text, 240),
			Status:   status,
			ChunkIDs: ids,
			Reason:   clipRunes(reason, 180),
		})
	}
	return validated, nil
}

func appendDiagnosisClaimReason(reason, suffix string) string {
	reason = strings.TrimSpace(reason)
	suffix = strings.TrimSpace(suffix)
	if reason == "" {
		return suffix
	}
	if suffix == "" {
		return reason
	}
	return reason + "; " + suffix
}

func evidenceScoreBucket(score float64) string {
	switch {
	case score >= 0.85:
		return "high"
	case score >= 0.55:
		return "medium"
	case score > 0:
		return "low"
	default:
		return ""
	}
}

// ValidateNoRawEvidenceLeak rejects text that contains substantial raw KB body
// content from the supplied hits. It permits safe ledger fields such as chunk_id
// and title.
func ValidateNoRawEvidenceLeak(text string, hits []RetrievalHit) error {
	haystack := strings.ToLower(compactWhitespace(text))
	if haystack == "" {
		return nil
	}
	for _, hit := range hits {
		for _, needle := range rawLeakNeedles(hit.Chunk.Content) {
			if strings.Contains(haystack, strings.ToLower(needle)) {
				chunkID := strings.TrimSpace(hit.Chunk.ChunkID)
				if chunkID == "" {
					chunkID = "unknown"
				}
				return fmt.Errorf("raw knowledge evidence leaked from chunk %s", chunkID)
			}
		}
	}
	return nil
}

func rawLeakNeedles(content string) []string {
	clean := compactWhitespace(content)
	if utf8.RuneCountInString(clean) < 32 {
		return nil
	}
	needles := []string{clean}
	for _, part := range strings.FieldsFunc(clean, func(r rune) bool {
		switch r {
		case '.', '!', '?', ';', '。', '！', '？', '；', '\n', '\r':
			return true
		default:
			return false
		}
	}) {
		part = strings.TrimSpace(part)
		if utf8.RuneCountInString(part) >= 32 {
			needles = append(needles, part)
		}
	}
	if utf8.RuneCountInString(clean) > 96 {
		needles = append(needles, clipRunes(clean, 96))
	}
	return dedupeStrings(needles)
}

func compactWhitespace(s string) string {
	return strings.Join(strings.Fields(strings.TrimSpace(s)), " ")
}

func clipRunes(s string, max int) string {
	if max <= 0 || utf8.RuneCountInString(s) <= max {
		return s
	}
	var b strings.Builder
	count := 0
	for _, r := range s {
		if count >= max {
			break
		}
		b.WriteRune(r)
		count++
	}
	return strings.TrimSpace(b.String())
}

func dedupeStrings(values []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func trimStrings(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}
