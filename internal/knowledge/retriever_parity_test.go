package knowledge

import (
	"bufio"
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
	retriever := NewRetriever(corpus, RetrieverOptions{
		TopK: 3,
		Now: func() time.Time {
			return time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC)
		},
	})
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
