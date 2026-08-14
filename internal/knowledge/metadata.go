package knowledge

import (
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/compshare-agent/internal/entity"
)

// metadataExactPoolSize caps the structured candidate leg before it joins RRF.
// It is deliberately independent of the BM25/dense pools: a matching model
// name or error code should provide an additional retrieval signal, not turn a
// metadata match into an unbounded or hard-filtered result set.
const metadataExactPoolSize = 20

type metadataTermHit struct {
	chunkIndex int
	weight     int
}

// metadataExactIndex is an in-memory projection of V2 provenance fields. It
// avoids adding a database/index dependency for the current small corpus while
// keeping the query-time contract explicit: only high-specificity identifiers
// and consciously supplied ExactTerms may produce this candidate list.
//
// The index intentionally complements BM25 and dense retrieval. A malformed
// metadata term, a source typo, or a broad number can never suppress normal
// candidates because callers use the result as a third RRF input, not a filter.
type metadataExactIndex struct {
	byTerm       map[string][]metadataTermHit
	explicitTerm map[string]struct{}
}

var knownTechnicalMetadataTerms = map[string]struct{}{
	"api": {}, "cli": {}, "comfyui": {}, "cuda": {}, "docker": {},
	"linux": {}, "pytorch": {}, "sdk": {}, "sglang": {}, "ssh": {},
	"tensorflow": {}, "vllm": {}, "windows": {},
}

func newMetadataExactIndex(chunks []KBChunk) metadataExactIndex {
	index := metadataExactIndex{
		byTerm:       make(map[string][]metadataTermHit),
		explicitTerm: make(map[string]struct{}),
	}
	for chunkIndex, chunk := range chunks {
		// Keep the highest field weight when the same term occurs more than
		// once in one chunk. This prevents verbose content from amplifying a
		// single identifier merely by repeating it.
		terms := make(map[string]int)
		addDerived := func(values []string, weight int) {
			for _, value := range values {
				for _, term := range metadataTermsFromText(value) {
					if weight > terms[term] {
						terms[term] = weight
					}
				}
			}
		}
		addExplicit := func(values []string, weight int) {
			for _, value := range values {
				term := normalizeMetadataText(value)
				if !metadataExplicitTermUsable(term) {
					continue
				}
				index.explicitTerm[term] = struct{}{}
				if weight > terms[term] {
					terms[term] = weight
				}
			}
		}

		addExplicit(chunk.ExactTerms, 4)
		addDerived([]string{chunk.Title, chunk.DocumentTitle}, 3)
		addDerived(chunk.HeadingPath, 2)
		addDerived(chunk.SourceRefs, 1)
		addDerived(chunk.QuestionPatterns, 1)
		// Existing V2 releases predate ExactTerms. A bounded content scan
		// lets their model IDs and error codes participate immediately, so a
		// full corpus re-preprocess is not a prerequisite for this feature.
		addDerived([]string{chunk.Content}, 1)

		for term, weight := range terms {
			index.byTerm[term] = append(index.byTerm[term], metadataTermHit{
				chunkIndex: chunkIndex,
				weight:     weight,
			})
		}
	}
	return index
}

func (index metadataExactIndex) candidates(question string, corpus []KBChunk, now time.Time) []scoredChunk {
	if len(index.byTerm) == 0 {
		return nil
	}

	matchedTerms := make(map[string]struct{})
	for _, term := range metadataTermsFromText(question) {
		if _, ok := index.byTerm[term]; ok {
			matchedTerms[term] = struct{}{}
		}
	}
	// Explicit terms may contain CJK product names or multi-token model
	// labels such as "RTX 4090". They are bounded at load time, and the
	// current corpus is small, so checking this curated set is cheap.
	normalizedQuestion := normalizeMetadataText(question)
	for term := range index.explicitTerm {
		if metadataTermOccurs(normalizedQuestion, term) {
			matchedTerms[term] = struct{}{}
		}
	}
	if len(matchedTerms) == 0 {
		return nil
	}

	scores := make(map[int]int)
	for term := range matchedTerms {
		for _, hit := range index.byTerm[term] {
			if hit.chunkIndex < 0 || hit.chunkIndex >= len(corpus) {
				continue
			}
			chunk := corpus[hit.chunkIndex]
			if !chunkActiveAt(chunk, now) || chunk.Confidence == confidenceLow {
				continue
			}
			scores[hit.chunkIndex] += hit.weight
		}
	}

	candidates := make([]scoredChunk, 0, len(scores))
	for chunkIndex, score := range scores {
		candidates = append(candidates, scoredChunk{
			chunk: corpus[chunkIndex],
			score: float64(score),
		})
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].score != candidates[j].score {
			return candidates[i].score > candidates[j].score
		}
		if confidenceRank(candidates[i].chunk.Confidence) != confidenceRank(candidates[j].chunk.Confidence) {
			return confidenceRank(candidates[i].chunk.Confidence) > confidenceRank(candidates[j].chunk.Confidence)
		}
		return candidates[i].chunk.ChunkID < candidates[j].chunk.ChunkID
	})
	if len(candidates) > metadataExactPoolSize {
		candidates = candidates[:metadataExactPoolSize]
	}
	return candidates
}

// metadataTermsFromText extracts only terms that are specific enough to be an
// auxiliary retrieval signal: identifiers containing a number or a separator,
// CamelCase names, and a deliberately small technical vocabulary. It does not
// emit ordinary natural-language words, which is what keeps a short question
// such as "怎么收费" on the normal BM25+dense path.
func metadataTermsFromText(value string) []string {
	// Chinese prose often touches an ASCII identifier without whitespace (for
	// example "使用ComfyUI工作流"). Scan ASCII identifier runs explicitly so
	// the metadata key is ComfyUI rather than the whole mixed-script sentence.
	tokens := metadataASCIIIdentifierTokens(value)
	seen := make(map[string]struct{}, len(tokens))
	terms := make([]string, 0, len(tokens))
	for _, token := range tokens {
		term := normalizeMetadataText(token)
		if !metadataDerivedTermUsable(token, term) {
			continue
		}
		if _, ok := seen[term]; ok {
			continue
		}
		seen[term] = struct{}{}
		terms = append(terms, term)
	}
	return terms
}

func metadataASCIIIdentifierTokens(value string) []string {
	var tokens []string
	var current strings.Builder
	flush := func() {
		if current.Len() > 0 {
			tokens = append(tokens, current.String())
			current.Reset()
		}
	}
	for _, r := range value {
		if isMetadataASCIIAlphaNumeric(r) {
			current.WriteRune(r)
			continue
		}
		if current.Len() > 0 && strings.ContainsRune("-_/.:", r) {
			current.WriteRune(r)
			continue
		}
		flush()
	}
	flush()
	return tokens
}

func isMetadataASCIIAlphaNumeric(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
}

func metadataDerivedTermUsable(raw, normalized string) bool {
	if normalized == "" || len([]rune(normalized)) < 2 || len([]rune(normalized)) > MaxExactTermRunes {
		return false
	}
	if metadataLooksLikeURL(normalized) {
		return false
	}
	hasASCII := false
	hasDigit := false
	hasLetter := false
	hasSeparator := false
	hasLower := false
	hasInternalUpper := false
	for index, r := range raw {
		if r <= unicode.MaxASCII && (unicode.IsLetter(r) || unicode.IsDigit(r)) {
			hasASCII = true
		}
		if unicode.IsLetter(r) {
			hasLetter = true
		}
		if unicode.IsDigit(r) {
			hasDigit = true
		}
		if strings.ContainsRune("-_/.:", r) {
			hasSeparator = true
		}
		if unicode.IsLower(r) {
			hasLower = true
		}
		if index > 0 && unicode.IsUpper(r) && hasLower {
			hasInternalUpper = true
		}
	}
	if !hasASCII {
		return false
	}
	// List markers such as "1." must not become an exact retrieval term.
	// Pure numbers are useful only once they are long enough to resemble an
	// error code/model number; numeric units (24GB) have letters and remain.
	if hasDigit && !hasLetter && len([]rune(normalized)) < 3 {
		return false
	}
	if hasDigit || hasSeparator || hasInternalUpper {
		return true
	}
	_, known := knownTechnicalMetadataTerms[normalized]
	return known
}

func metadataExplicitTermUsable(term string) bool {
	if term == "" || len([]rune(term)) < 2 || len([]rune(term)) > MaxExactTermRunes {
		return false
	}
	return true
}

func metadataLooksLikeURL(value string) bool {
	runes := []rune(value)
	for index := 0; index+2 < len(runes); index++ {
		if runes[index] == ':' && runes[index+1] == '/' && runes[index+2] == '/' {
			return true
		}
	}
	return false
}

// metadataTermOccurs avoids treating a numeric/model prefix as an exact hit:
// "100" must not match "1000", and "A100" must not match "A1000". CJK
// curated terms intentionally use substring semantics because Chinese does not
// have whitespace word boundaries and ExactTerms are an explicit source field.
func metadataTermOccurs(question, term string) bool {
	return entity.TextExplicitlyMentionsName(question, term)
}

// normalizeMetadataText retains identifier punctuation that NormalizeQuery
// intentionally strips. Exact model/API identifiers therefore remain distinct
// (qwen3-reranker-8b vs qwen3 reranker 8b) while whitespace is canonicalized
// for curated multi-token ExactTerms.
func normalizeMetadataText(value string) string {
	var builder strings.Builder
	spacePending := false
	for _, r := range strings.TrimSpace(value) {
		switch {
		case unicode.IsLetter(r), unicode.IsDigit(r):
			if spacePending && builder.Len() > 0 {
				builder.WriteByte(' ')
			}
			builder.WriteRune(unicode.ToLower(r))
			spacePending = false
		case strings.ContainsRune("-_/.:", r):
			builder.WriteRune(r)
			spacePending = false
		case unicode.IsSpace(r):
			spacePending = true
		default:
			spacePending = true
		}
	}
	return strings.Trim(strings.TrimSpace(builder.String()), "-_/.: ")
}
