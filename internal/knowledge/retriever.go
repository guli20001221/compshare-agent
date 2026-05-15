package knowledge

import (
	"sort"
	"strings"
	"time"
)

const (
	defaultRetrieverTopK      = 3
	defaultRetrieverThreshold = 0.5
)

var beijingLocation = time.FixedZone("Asia/Shanghai", 8*60*60)

type RetrieverOptions struct {
	TopK      int
	Threshold float64
	Now       func() time.Time
}

type RetrievalResult struct {
	Enabled         bool
	KBVersion       string
	QueryNormalized string
	Hits            []KBChunk
	HitItems        []RetrievalHit
	Empty           bool
}

type RetrievalHit struct {
	Chunk KBChunk
	Score float64
	Kept  bool
}

type Retriever struct {
	corpus    Corpus
	topK      int
	threshold float64
	now       func() time.Time
	bm25      retrievalBM25Index
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
	return &Retriever{corpus: corpus, topK: topK, threshold: threshold, now: now, bm25: newRetrievalBM25Index(corpus.Chunks)}
}

func (r *Retriever) Retrieve(question, productArea string) RetrievalResult {
	queryTokens := tokenizeRetrievalText(question)
	queryNormalized := NormalizeQuery(question)
	productArea = strings.TrimSpace(strings.ToLower(productArea))
	candidates := make([]scoredChunk, 0, len(r.corpus.Chunks))
	for index, chunk := range r.corpus.Chunks {
		if !chunkActiveAt(chunk, r.now()) || chunk.Confidence == confidenceLow {
			continue
		}
		score := r.scoreChunk(queryTokens, productArea, index, chunk)
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
	hitItems := make([]RetrievalHit, 0, len(candidates))
	for _, candidate := range candidates {
		hits = append(hits, candidate.chunk)
		hitItems = append(hitItems, RetrievalHit{
			Chunk: candidate.chunk,
			Score: candidate.score,
			Kept:  true,
		})
	}
	return RetrievalResult{
		Enabled:         true,
		KBVersion:       r.corpus.KBVersion,
		QueryNormalized: queryNormalized,
		Hits:            hits,
		HitItems:        hitItems,
		Empty:           len(hits) == 0,
	}
}

type scoredChunk struct {
	chunk KBChunk
	score float64
}

func (r *Retriever) scoreChunk(queryTokens []string, productArea string, chunkIndex int, chunk KBChunk) float64 {
	if len(queryTokens) == 0 {
		return 0
	}
	score := patternsFieldWeight*r.bm25.patterns.score(chunkIndex, queryTokens) +
		titleFieldWeight*r.bm25.titles.score(chunkIndex, queryTokens) +
		contentFieldWeight*r.bm25.contents.score(chunkIndex, queryTokens)
	if score <= 0 {
		return 0
	}
	if productArea != "" && strings.EqualFold(productArea, chunk.ProductArea) {
		score += 2
	}
	return score
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
