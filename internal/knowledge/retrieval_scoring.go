package knowledge

import (
	"math"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

const (
	bm25K1              = 1.5
	bm25B               = 0.75
	patternsFieldWeight = 4.0
	titleFieldWeight    = 3.0
	contentFieldWeight  = 1.0
)

type retrievalBM25Index struct {
	patterns bm25FieldIndex
	titles   bm25FieldIndex
	contents bm25FieldIndex
}

type bm25FieldIndex struct {
	documents []bm25Document
	idf       map[string]float64
	avgLength float64
}

type bm25Document struct {
	termFrequency map[string]int
	length        int
}

func newRetrievalBM25Index(chunks []KBChunk) retrievalBM25Index {
	patterns := make([]string, 0, len(chunks))
	titles := make([]string, 0, len(chunks))
	contents := make([]string, 0, len(chunks))
	for _, chunk := range chunks {
		patterns = append(patterns, strings.Join(chunk.QuestionPatterns, " "))
		titles = append(titles, chunk.Title)
		contents = append(contents, chunk.Content)
	}
	return retrievalBM25Index{
		patterns: newBM25FieldIndex(patterns),
		titles:   newBM25FieldIndex(titles),
		contents: newBM25FieldIndex(contents),
	}
}

func newBM25FieldIndex(values []string) bm25FieldIndex {
	documents := make([]bm25Document, 0, len(values))
	documentFrequency := map[string]int{}
	totalLength := 0
	for _, value := range values {
		tokens := tokenizeRetrievalText(value)
		document := bm25Document{termFrequency: map[string]int{}, length: len(tokens)}
		seen := map[string]struct{}{}
		for _, token := range tokens {
			document.termFrequency[token]++
			seen[token] = struct{}{}
		}
		for token := range seen {
			documentFrequency[token]++
		}
		totalLength += document.length
		documents = append(documents, document)
	}
	avgLength := 1.0
	if len(documents) > 0 && totalLength > 0 {
		avgLength = float64(totalLength) / float64(len(documents))
	}
	idf := map[string]float64{}
	n := float64(len(documents))
	for token, df := range documentFrequency {
		idf[token] = math.Log((n-float64(df)+0.5)/(float64(df)+0.5) + 1)
	}
	return bm25FieldIndex{documents: documents, idf: idf, avgLength: avgLength}
}

func (field bm25FieldIndex) score(chunkIndex int, queryTokens []string) float64 {
	if chunkIndex < 0 || chunkIndex >= len(field.documents) {
		return 0
	}
	document := field.documents[chunkIndex]
	if document.length == 0 {
		return 0
	}
	score := 0.0
	for _, token := range queryTokens {
		tf := document.termFrequency[token]
		if tf == 0 {
			continue
		}
		idf := field.idf[token]
		tfFloat := float64(tf)
		denominator := tfFloat + bm25K1*(1-bm25B+bm25B*float64(document.length)/field.avgLength)
		if denominator == 0 {
			continue
		}
		score += idf * (tfFloat * (bm25K1 + 1)) / denominator
	}
	return score
}

func tokenizeRetrievalText(value string) []string {
	normalized := NormalizeQuery(value)
	if normalized == "" {
		return nil
	}
	var tokens []string
	for _, segment := range strings.Fields(normalized) {
		if segment == "" {
			continue
		}
		if isASCIIAlnumSegment(segment) {
			tokens = append(tokens, segment)
			continue
		}
		runes := []rune(segment)
		for n := 2; n <= 3; n++ {
			if len(runes) < n {
				continue
			}
			for index := 0; index <= len(runes)-n; index++ {
				tokens = append(tokens, string(runes[index:index+n]))
			}
		}
	}
	return tokens
}

// NormalizeQuery is shared by the runtime retriever and eval parity tests; keep
// its preprocessing semantics aligned with the BM25 scorer in this file.
func NormalizeQuery(value string) string {
	value = strings.ToLower(norm.NFKC.String(value))
	var builder strings.Builder
	lastWasSpace := true
	for _, r := range value {
		switch {
		case isASCIIAlnum(r) || isCJK(r):
			builder.WriteRune(r)
			lastWasSpace = false
		case unicode.IsSpace(r):
			if !lastWasSpace {
				builder.WriteByte(' ')
				lastWasSpace = true
			}
		default:
			// Punctuation and symbols are intentionally dropped.
		}
	}
	return strings.TrimSpace(builder.String())
}

func isASCIIAlnumSegment(value string) bool {
	for _, r := range value {
		if !isASCIIAlnum(r) {
			return false
		}
	}
	return value != ""
}

func isASCIIAlnum(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
}

func isCJK(r rune) bool {
	for _, rng := range cjkRanges {
		if r >= rng.lo && r <= rng.hi {
			return true
		}
	}
	return false
}

var cjkRanges = []struct {
	lo rune
	hi rune
}{
	{0x3400, 0x4DBF},
	{0x4E00, 0x9FFF},
	{0xF900, 0xFAFF},
	{0x20000, 0x2A6DF},
	{0x2A700, 0x2EBEF},
	{0x30000, 0x3134F},
}
