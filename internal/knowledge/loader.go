package knowledge

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	MaxQuestionPatterns        = 20
	MaxQuestionPatternRunes    = 200
	MaxKnowledgeContentRunes   = 4000
	customerSafeACL            = "customer_safe"
	confidenceHigh             = "high"
	confidenceMedium           = "medium"
	confidenceLow              = "low"
	sourceTypeFAQ              = "faq"
	sourceTypeRunbook          = "runbook"
	defaultCorpusScannerBuffer = 64 * 1024
)

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
