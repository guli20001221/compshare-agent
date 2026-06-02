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
	ChunkID string `json:"chunk_id"`
	Title   string `json:"title,omitempty"`
	Summary string `json:"summary"`
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
			ChunkID: chunkID,
			Title:   title,
			Summary: clipRunes(summary, 160),
		})
		if len(ledger.Items) >= maxItems {
			break
		}
	}
	return ledger
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
