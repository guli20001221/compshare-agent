package knowledge

import (
	"sort"
	"strings"
	"time"
)

const (
	defaultRetrieverTopK      = 3
	defaultRetrieverThreshold = 2
)

var beijingLocation = time.FixedZone("Asia/Shanghai", 8*60*60)

type RetrieverOptions struct {
	TopK      int
	Threshold int
	Now       func() time.Time
}

type RetrievalResult struct {
	Enabled   bool
	KBVersion string
	Hits      []KBChunk
	Empty     bool
}

type Retriever struct {
	corpus    Corpus
	topK      int
	threshold int
	now       func() time.Time
}

func NewRetriever(corpus Corpus, opts RetrieverOptions) *Retriever {
	topK := opts.TopK
	if topK <= 0 {
		topK = defaultRetrieverTopK
	}
	threshold := opts.Threshold
	if threshold <= 0 {
		threshold = defaultRetrieverThreshold
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Retriever{corpus: corpus, topK: topK, threshold: threshold, now: now}
}

func (r *Retriever) Retrieve(question, productArea string) RetrievalResult {
	candidates := make([]scoredChunk, 0, len(r.corpus.Chunks))
	for _, chunk := range r.corpus.Chunks {
		if !chunkActiveAt(chunk, r.now()) || chunk.Confidence == confidenceLow {
			continue
		}
		score := scoreChunk(question, productArea, chunk)
		if score < r.threshold {
			continue
		}
		candidates = append(candidates, scoredChunk{chunk: chunk, score: score})
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		left, right := candidates[i], candidates[j]
		if left.score != right.score {
			return left.score > right.score
		}
		if confidenceRank(left.chunk.Confidence) != confidenceRank(right.chunk.Confidence) {
			return confidenceRank(left.chunk.Confidence) > confidenceRank(right.chunk.Confidence)
		}
		return left.chunk.ChunkID < right.chunk.ChunkID
	})
	if len(candidates) > r.topK {
		candidates = candidates[:r.topK]
	}
	hits := make([]KBChunk, 0, len(candidates))
	for _, candidate := range candidates {
		hits = append(hits, candidate.chunk)
	}
	return RetrievalResult{
		Enabled:   true,
		KBVersion: r.corpus.KBVersion,
		Hits:      hits,
		Empty:     len(hits) == 0,
	}
}

type scoredChunk struct {
	chunk KBChunk
	score int
}

func scoreChunk(question, productArea string, chunk KBChunk) int {
	question = strings.TrimSpace(strings.ToLower(question))
	productArea = strings.TrimSpace(strings.ToLower(productArea))
	score := 0
	for _, pattern := range chunk.QuestionPatterns {
		if textMatches(question, pattern) {
			score += 4
			break
		}
	}
	if textMatches(question, chunk.Title) {
		score += 3
	}
	if productArea != "" && strings.EqualFold(productArea, chunk.ProductArea) {
		score += 2
	}
	if textMatches(question, chunk.Content) {
		score++
	}
	return score
}

func textMatches(question, field string) bool {
	field = strings.TrimSpace(strings.ToLower(field))
	if question == "" || field == "" {
		return false
	}
	return strings.Contains(question, field) || strings.Contains(field, question)
}

func chunkActiveAt(chunk KBChunk, now time.Time) bool {
	today := dateOnlyBeijing(now)
	if chunk.ValidFrom != "" {
		validFrom, err := time.ParseInLocation("2006-01-02", chunk.ValidFrom, beijingLocation)
		if err != nil || today.Before(validFrom) {
			return false
		}
	}
	if chunk.ValidTo != nil && strings.TrimSpace(*chunk.ValidTo) != "" {
		validTo, err := time.ParseInLocation("2006-01-02", *chunk.ValidTo, beijingLocation)
		if err != nil || today.After(validTo) {
			return false
		}
	}
	return true
}

func confidenceRank(confidence string) int {
	switch confidence {
	case confidenceHigh:
		return 2
	case confidenceMedium:
		return 1
	default:
		return 0
	}
}

func dateOnlyBeijing(t time.Time) time.Time {
	year, month, day := t.In(beijingLocation).Date()
	return time.Date(year, month, day, 0, 0, 0, 0, beijingLocation)
}
