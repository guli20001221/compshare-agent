package knowledge

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/compshare-agent/internal/envelope"
)

const (
	MaxQuestionPatterns           = 20
	MaxQuestionPatternRunes       = 200
	MaxKnowledgeContentRunes      = 4000
	MaxHeadingPathEntries         = 20
	MaxHeadingPathRunes           = 300
	MaxSourceRefs                 = 16
	MaxSourceRefRunes             = 600
	MaxExactTerms                 = 32
	MaxExactTermRunes             = 200
	customerSafeACL               = "customer_safe"
	confidenceHigh                = "high"
	confidenceMedium              = "medium"
	confidenceLow                 = "low"
	sourceTypeFAQ                 = "faq"
	sourceTypeRunbook             = "runbook"
	sourceOriginOfficial          = "official"
	sourceOriginSupportCurated    = "support_curated"
	sourceOriginExternalOfficial  = "external_official"
	sourceOriginExternalCommunity = "external_community"
	defaultCorpusScannerBuffer    = 64 * 1024
)

// allowedSourceOrigins is the Go side of a cross-language enum contract: this
// set must stay aligned with scripts/rag_w0/common.py ALLOWED_SOURCE_ORIGINS
// (asserted by TestSourceOriginEnumMatchesPython). The external_* values are
// reserved for the out-of-platform tool/ops corpus (deploy/kb/external_w0.jsonl);
// platform chunks stay official / support_curated.
var allowedSourceOrigins = map[string]struct{}{
	sourceOriginOfficial:          {},
	sourceOriginSupportCurated:    {},
	sourceOriginExternalOfficial:  {},
	sourceOriginExternalCommunity: {},
}

func LoadCorpus(path string) (Corpus, error) {
	f, err := os.Open(path)
	if err != nil {
		return Corpus{}, fmt.Errorf("open knowledge corpus: %w", err)
	}
	defer f.Close()

	var corpus Corpus
	seenChunkIDs := map[string]struct{}{}
	scanner := bufio.NewScanner(f)
	// Current chunk bounds fit under 64KB with headroom:
	// content 4000 runes + 20 patterns * 200 runes + JSON overhead.
	scanner.Buffer(make([]byte, 1024), defaultCorpusScannerBuffer)
	row := 0
	for scanner.Scan() {
		row++
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var chunk KBChunk
		if err := json.Unmarshal([]byte(line), &chunk); err != nil {
			return Corpus{}, fmt.Errorf("row %d: parse JSON: %w", row, err)
		}
		if err := validateChunk(chunk); err != nil {
			return Corpus{}, fmt.Errorf("row %d: %w", row, err)
		}
		if _, ok := seenChunkIDs[chunk.ChunkID]; ok {
			return Corpus{}, fmt.Errorf("row %d: duplicate chunk_id %q", row, chunk.ChunkID)
		}
		seenChunkIDs[chunk.ChunkID] = struct{}{}
		if corpus.KBVersion == "" {
			corpus.KBVersion = chunk.KBVersion
		} else if corpus.KBVersion != chunk.KBVersion {
			return Corpus{}, fmt.Errorf("row %d: kb_version %q does not match corpus version %q", row, chunk.KBVersion, corpus.KBVersion)
		}
		corpus.Chunks = append(corpus.Chunks, chunk)
	}
	if err := scanner.Err(); err != nil {
		return Corpus{}, fmt.Errorf("read knowledge corpus: %w", err)
	}
	if len(corpus.Chunks) == 0 {
		return Corpus{}, fmt.Errorf("empty corpus")
	}
	return corpus, nil
}

func validateChunk(chunk KBChunk) error {
	required := []struct {
		name  string
		value string
	}{
		{"chunk_id", chunk.ChunkID},
		{"kb_version", chunk.KBVersion},
		{"source_type", chunk.SourceType},
		{"source_origin", chunk.SourceOrigin},
		{"product_area", chunk.ProductArea},
		{"acl", chunk.ACL},
		{"confidence", chunk.Confidence},
		{"title", chunk.Title},
		{"content", chunk.Content},
	}
	for _, field := range required {
		if strings.TrimSpace(field.value) == "" {
			return fmt.Errorf("missing required field %s", field.name)
		}
	}
	if chunk.ACL != customerSafeACL {
		return fmt.Errorf("acl must be %q", customerSafeACL)
	}
	switch chunk.SourceType {
	case sourceTypeFAQ, sourceTypeRunbook:
	default:
		return fmt.Errorf("source_type must be faq or runbook")
	}
	if _, ok := allowedSourceOrigins[chunk.SourceOrigin]; !ok {
		return fmt.Errorf("source_origin must be one of official, support_curated, external_official, external_community")
	}
	switch chunk.Confidence {
	case confidenceHigh, confidenceMedium, confidenceLow:
	default:
		return fmt.Errorf("confidence must be high, medium, or low")
	}
	if err := validateOptionalDate("valid_from", chunk.ValidFrom); err != nil {
		return err
	}
	if chunk.ValidTo != nil {
		if err := validateOptionalDate("valid_to", *chunk.ValidTo); err != nil {
			return err
		}
	}
	if chunk.SurfaceURL != nil {
		decision := envelope.IsAllowedSurfaceURL(strings.TrimSpace(*chunk.SurfaceURL))
		if !decision.Allowed {
			return fmt.Errorf("surface_url rejected by policy: %s", decision.Reason)
		}
	}
	if len(chunk.QuestionPatterns) > MaxQuestionPatterns {
		return fmt.Errorf("question_patterns must contain at most %d entries", MaxQuestionPatterns)
	}
	for i, pattern := range chunk.QuestionPatterns {
		if utf8.RuneCountInString(pattern) > MaxQuestionPatternRunes {
			return fmt.Errorf("question_patterns[%d] exceeds %d runes", i, MaxQuestionPatternRunes)
		}
	}
	if utf8.RuneCountInString(chunk.Content) > MaxKnowledgeContentRunes {
		return fmt.Errorf("content exceeds %d runes", MaxKnowledgeContentRunes)
	}
	if err := validateOptionalStringSlice("heading_path", chunk.HeadingPath, MaxHeadingPathEntries, MaxHeadingPathRunes, false); err != nil {
		return err
	}
	if err := validateOptionalStringSlice("source_refs", chunk.SourceRefs, MaxSourceRefs, MaxSourceRefRunes, true); err != nil {
		return err
	}
	if err := validateOptionalStringSlice("exact_terms", chunk.ExactTerms, MaxExactTerms, MaxExactTermRunes, true); err != nil {
		return err
	}
	if chunk.ChunkOrdinal < 0 {
		return fmt.Errorf("chunk_ordinal must be non-negative")
	}
	if strings.TrimSpace(chunk.ParentID) != "" && strings.TrimSpace(chunk.DocumentID) == "" {
		return fmt.Errorf("parent_id requires document_id")
	}
	return nil
}

func validateOptionalStringSlice(field string, values []string, maxEntries, maxRunes int, requireUnique bool) error {
	if len(values) > maxEntries {
		return fmt.Errorf("%s must contain at most %d entries", field, maxEntries)
	}
	seen := make(map[string]struct{}, len(values))
	for index, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			return fmt.Errorf("%s[%d] must not be empty", field, index)
		}
		if utf8.RuneCountInString(value) > maxRunes {
			return fmt.Errorf("%s[%d] exceeds %d runes", field, index, maxRunes)
		}
		if _, ok := seen[value]; ok && requireUnique {
			return fmt.Errorf("%s[%d] duplicates an earlier entry", field, index)
		}
		seen[value] = struct{}{}
	}
	return nil
}

func validateOptionalDate(field, value string) error {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	if _, err := time.Parse("2006-01-02", value); err != nil {
		return fmt.Errorf("%s must use YYYY-MM-DD format", field)
	}
	return nil
}
