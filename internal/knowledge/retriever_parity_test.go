package knowledge

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"
)

type parityQuestion struct {
	QuestionID       string `json:"question_id"`
	Question         string `json:"question"`
	ProductArea      string `json:"product_area"`
	ExpectedBehavior string `json:"expected_behavior"`
}

func TestRetrieverParityFixture(t *testing.T) {
	chunksPath := os.Getenv("RAG_RETRIEVER_PARITY_CHUNKS")
	questionsPath := os.Getenv("RAG_RETRIEVER_PARITY_QUESTIONS")
	outPath := os.Getenv("RAG_RETRIEVER_PARITY_OUT")
	if chunksPath == "" || questionsPath == "" || outPath == "" {
		t.Skip("set RAG_RETRIEVER_PARITY_CHUNKS, RAG_RETRIEVER_PARITY_QUESTIONS, and RAG_RETRIEVER_PARITY_OUT to run parity fixture")
	}

	corpus, err := LoadCorpus(chunksPath)
	if err != nil {
		t.Fatalf("load corpus: %v", err)
	}
	questions, err := loadParityQuestions(questionsPath)
	if err != nil {
		t.Fatalf("load questions: %v", err)
	}

	embeddingsPath := os.Getenv("RAG_RETRIEVER_PARITY_EMBEDDINGS")
	queryCachePath := os.Getenv("RAG_RETRIEVER_PARITY_QUERY_CACHE")
	// The fixture date must be on/after the latest chunk.valid_from in the
	// pinned corpus; the Python evaluate_retrieval pipeline does not apply
	// the valid_from filter, so an earlier Now() here silently drops 65+
	// chunks on the Go side and parity diverges. Anchor to the day after
	// the latest known valid_from (2026-05-17) so any future RAG-11 batch
	// addition shows up as a fixture-date update reminder.
	opts := RetrieverOptions{
		TopK: 3,
		Now: func() time.Time {
			return time.Date(2026, 5, 18, 12, 0, 0, 0, time.UTC)
		},
	}
	// Hybrid parity: same BM25 top-20 -> embedding rerank as the Python eval.
	// Requires the embeddings sidecar (corpus side) + the question_id-keyed
	// query embedding cache produced by evaluate_retrieval.py --mode hybrid.
	if embeddingsPath != "" && queryCachePath != "" {
		sidecar, err := LoadEmbeddingSidecar(embeddingsPath)
		if err != nil {
			t.Fatalf("load embedding sidecar: %v", err)
		}
		cache, err := loadParityQueryEmbeddings(queryCachePath)
		if err != nil {
			t.Fatalf("load query embedding cache: %v", err)
		}
		opts.EmbeddingSidecar = &sidecar
		opts.Embedder = &fixtureQueryEmbedder{byQuestion: cache}
	}

	retriever := NewRetriever(corpus, opts)
	out := map[string][]string{}
	for _, question := range questions {
		result := retriever.Retrieve(question.Question, question.ProductArea)
		ids := make([]string, 0, len(result.Hits))
		for _, hit := range result.Hits {
			ids = append(ids, hit.ChunkID)
		}
		out[question.QuestionID] = ids
	}
	payload, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		t.Fatalf("marshal parity output: %v", err)
	}
	if err := os.WriteFile(outPath, append(payload, '\n'), 0o644); err != nil {
		t.Fatalf("write parity output: %v", err)
	}
}

// fixtureQueryEmbedder serves precomputed query embeddings keyed by question
// text. It mirrors what scripts/rag_w0/evaluate_retrieval.py:_QueryEmbedder
// reads from --query-embedding-cache so Go-Python parity tests don't have to
// call ModelVerse at test time.
type fixtureQueryEmbedder struct {
	byQuestion map[string][]float32
}

func (e *fixtureQueryEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	if v, ok := e.byQuestion[text]; ok {
		return v, nil
	}
	return nil, nil // empty vector -> retriever falls back to BM25 path
}

func loadParityQueryEmbeddings(path string) (map[string][]float32, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := map[string][]float32{}
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024), 256*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var row struct {
			QuestionID string    `json:"question_id"`
			Question   string    `json:"question"` // optional but present in eval cache
			Vector     []float32 `json:"vector"`
		}
		if err := json.Unmarshal(line, &row); err != nil {
			return nil, err
		}
		key := row.Question
		if key == "" {
			key = row.QuestionID
		}
		out[key] = row.Vector
	}
	return out, scanner.Err()
}

func loadParityQuestions(path string) ([]parityQuestion, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var questions []parityQuestion
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var question parityQuestion
		if err := json.Unmarshal(line, &question); err != nil {
			return nil, err
		}
		questions = append(questions, question)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return questions, nil
}
