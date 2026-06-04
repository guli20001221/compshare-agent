package knowledge

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

const DefaultEvidenceLedgerMaxItems = 3

// EvidenceLedger is the safe view of retrieval results for body-read agent
// skills. It intentionally does not expose KBChunk.Content. Terminal RAG still
// uses the full cited prompt path; this ledger is for evidence-in-process only.
type EvidenceLedger struct {
	Query string         `json:"query,omitempty"`
	Items []EvidenceItem `json:"items"`
}

type EvidenceItem struct {
	ChunkID     string `json:"chunk_id"`
	Title       string `json:"title,omitempty"`
	SourceType  string `json:"source_type,omitempty"`
	ScoreBucket string `json:"score_bucket,omitempty"`
	Summary     string `json:"summary"`
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
